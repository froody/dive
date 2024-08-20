package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/wagoodman/dive/dive/filetree"
	"github.com/wagoodman/dive/dive/image"
	"github.com/klauspost/compress/zstd"
)

type ImageArchive struct {
	manifest manifest
	config   config
	layerMap map[string]*filetree.FileTree
}

func zstdReader(r io.Reader) (*tar.Reader, error) {
    zstd_reader, err := zstd.NewReader(r)

    if err != nil {
        return nil, err
    }

    return tar.NewReader(zstd_reader), nil
}

func gzipReader(r io.Reader) (*tar.Reader, error) {
    gzip_reader, err := gzip.NewReader(r)

    if err != nil {
        return nil, err
    }

    return tar.NewReader(gzip_reader), nil
}

type TreePair struct {
    tree *filetree.FileTree
    diffid string
    err error
}

func NewImageArchiveFromDir(directory string, manifest OciManifest, manifest_json []byte) (*ImageArchive, error) {
	img := &ImageArchive{
		layerMap: make(map[string]*filetree.FileTree),
	}

    ch := make(chan TreePair)

    digest_path := strings.ReplaceAll(manifest.Config.Digest, "sha256:", "")
    configReader, err := os.Open(path.Join(directory, digest_path))
    if err != nil {
        return img, err
    }

    configContent, err := io.ReadAll(configReader)
    if err != nil {
        return img, err
    }
    img.config = newConfig(configContent)
//    img.manifest = newManifest(manifest_json)

    for idx, layer := range manifest.Layers {
        go func(layer string, diffid string, media_type string) {
            layer_filename := strings.ReplaceAll(layer, "sha256:", "")
            layerReader, err := os.Open(path.Join(directory, layer_filename))

            if err != nil {
                ch <- TreePair{nil, diffid, err}
                return
            }

            if strings.HasSuffix(media_type, ".tar") {
                reader := tar.NewReader(layerReader)
                tree, err := processLayerTar(layer, reader)
                if err != nil {
                    ch <- TreePair{nil, diffid, err}
                } else {
                    ch <- TreePair{tree, diffid, nil}
                }
            } else if strings.HasSuffix(media_type, ".tar+gzip") || strings.HasSuffix(media_type, ".tar.gzip") {
                reader, err := gzipReader(layerReader)
                if err != nil {
                    ch <- TreePair{nil, diffid, err}
                    return
                }
                tree, err := processLayerTar(layer, reader)
                if err != nil {
                    ch <- TreePair{nil, diffid, err}
                } else {
                    ch <- TreePair{tree, diffid, nil}
                }
            } else if strings.HasSuffix(media_type, ".tar+zstd") || strings.HasSuffix(media_type, ".tar.zstd") {
                reader, err := zstdReader(layerReader)
                if err != nil {
                    ch <- TreePair{nil, diffid, err}
                    return
                }
                tree, err := processLayerTar(layer, reader)
                if err != nil {
                    ch <- TreePair{nil, diffid, err}
                } else {
                    ch <- TreePair{tree, diffid, nil}
                }
            } else {
                ch <- TreePair{nil, diffid, fmt.Errorf("unknown media type: %s", media_type)}
            }
        }(layer.Digest, img.config.RootFs.DiffIds[idx], layer.MediaType)
    }

    for i := 0; i < len(manifest.Layers); i++ {
        res := <-ch
        if res.err != nil {
            return img, res.err
        }
        fmt.Println("GotLayer: ", res.tree.Name)
        img.layerMap[res.diffid] = res.tree
    }


    fmt.Println("config: ", img.config)
    return img, nil
}

func NewImageArchive(tarFile io.ReadCloser) (*ImageArchive, error) {
	img := &ImageArchive{
		layerMap: make(map[string]*filetree.FileTree),
	}

	tarReader := tar.NewReader(tarFile)
    print("tarReader: ", tarReader, "\n")

    for {
        header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
        print("tar Name: ", header.Name, "\n")
    }
    //tarFile.Seek(0, 0)
    tarReader = tar.NewReader(tarFile)

	// store discovered json files in a map so we can read the image in one pass
	jsonFiles := make(map[string][]byte)

	var currentLayer uint
	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		name := header.Name

        print("tarf Name: ", name, "\n")

		// some layer tars can be relative layer symlinks to other layer tars
		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeReg {
			// For the Docker image format, use file name conventions
			if strings.HasSuffix(name, ".tar") {
				currentLayer++
				layerReader := tar.NewReader(tarReader)
				tree, err := processLayerTar(name, layerReader)
				if err != nil {
					return img, err
				}

				// add the layer to the image
				img.layerMap[tree.Name] = tree
			} else if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, "tgz") {
				currentLayer++

				// Add gzip reader
				gz, err := gzip.NewReader(tarReader)
				if err != nil {
					return img, err
				}

				// Add tar reader
				layerReader := tar.NewReader(gz)

				// Process layer
				tree, err := processLayerTar(name, layerReader)
				if err != nil {
					return img, err
				}

				// add the layer to the image
				img.layerMap[tree.Name] = tree
			} else if strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "sha256:") {
				fileBuffer, err := io.ReadAll(tarReader)
				if err != nil {
					return img, err
				}
				jsonFiles[name] = fileBuffer
			} else if strings.HasPrefix(name, "blobs/") {
                print("blobs: ", name, "\n")
				// For the OCI-compatible image format (used since Docker 25), use mime sniffing
				// but limit this to only the blobs/ (containing the config, and the layers)

				// The idea here is that we try various formats in turn, and those tries should
				// never consume more bytes than this buffer contains so we can start again.

				// 512 bytes ought to be enough (as that's the size of a TAR entry header),
				// but play it safe with 1024 bytes. This should also include very small layers
				// (unless they've also been gzipped, but Docker does not appear to do it)
				buffer := make([]byte, 1024)
				n, err := io.ReadFull(tarReader, buffer)
				if err != nil && err != io.ErrUnexpectedEOF {
                    print("blob err: ", name, ", ", err, "\n")
					return img, err
				}

                print("blob ncap: ", name, ", ", n, ", ", cap(buffer), "\n")

				// Only try reading a TAR if file is "big enough"
				//if n == cap(buffer) {
                {
                    print("blob cap: ", name,", ", n, "\n")
					var unwrappedReader io.Reader
					unwrappedReader, err = gzip.NewReader(io.MultiReader(bytes.NewReader(buffer[:n]), tarReader))
					if err != nil {
						// Not a gzipped entry
                        print("not gzip: ", name, "\n")
						unwrappedReader = io.MultiReader(bytes.NewReader(buffer[:n]), tarReader)
                        unwrappedReader, err = zstd.NewReader(unwrappedReader)
                        
                        if err != nil {
                            print("not zstd: ", name, "\n")
                            unwrappedReader = io.MultiReader(bytes.NewReader(buffer[:n]), tarReader)
                        } else {
                            print("zstd: ", name, "\n")
                        }
                    }

					// Try reading a TAR
					layerReader := tar.NewReader(unwrappedReader)
					tree, err := processLayerTar(name, layerReader)
					if err == nil {
                        print("blob tree: ", tree.Name, "\n")
						currentLayer++
						// add the layer to the image
						img.layerMap[tree.Name] = tree
						continue
					} else {
                        print("err tar: ", name, ", ", err, "\n")
                    }
				}

                print("blob other: \n")

				// Not a TAR (or smaller than our buffer), might be a JSON file
				decoder := json.NewDecoder(bytes.NewReader(buffer[:n]))
				token, err := decoder.Token()
				if _, ok := token.(json.Delim); err == nil && ok {
					// Looks like a JSON object (or array)
					// XXX: should we add a header.Size check too?
					fileBuffer, err := io.ReadAll(io.MultiReader(bytes.NewReader(buffer[:n]), tarReader))
					if err != nil {
						return img, err
					}
					jsonFiles[name] = fileBuffer
				}
				// Ignore every other unknown file type
			}
		}
	}

	manifestContent, exists := jsonFiles["manifest.json"]

    for k, v := range jsonFiles {
        print("jsonf ", k, "\n")
        print("jsonf ", string(v), "\n")
    }
	if !exists {
		return img, fmt.Errorf("could not find image manifest")
	}

    print("Found manifest\n")
    // get string from byte array
    stringManifestContent := string(manifestContent)

    print(stringManifestContent, "\n")

	img.manifest = newManifest(manifestContent)

	configContent, exists := jsonFiles[img.manifest.ConfigPath]
	if !exists {
		return img, fmt.Errorf("could not find image config")
	}

	img.config = newConfig(configContent)

	return img, nil
}

func processLayerTar(name string, reader *tar.Reader) (*filetree.FileTree, error) {
	tree := filetree.NewFileTree()
	tree.Name = name

	fileInfos, err := getFileList(reader)
	if err != nil {
		return nil, err
	}

	for _, element := range fileInfos {
		tree.FileSize += uint64(element.Size)

		_, _, err := tree.AddPath(element.Path, element)
		if err != nil {
			return nil, err
		}
	}

	return tree, nil
}

func getFileList(tarReader *tar.Reader) ([]filetree.FileInfo, error) {
	var files []filetree.FileInfo

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		// always ensure relative path notations are not parsed as part of the filename
		name := path.Clean(header.Name)
		if name == "." {
			continue
		}

		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			return nil, fmt.Errorf("unexptected tar file: (XGlobalHeader): type=%v name=%s", header.Typeflag, name)
		case tar.TypeXHeader:
			return nil, fmt.Errorf("unexptected tar file (XHeader): type=%v name=%s", header.Typeflag, name)
		default:
			files = append(files, filetree.NewFileInfoFromTarHeader(tarReader, header, name))
		}
	}
	return files, nil
}

func (img *ImageArchive) ToImage() (*image.Image, error) {
	trees := make([]*filetree.FileTree, 0)

	for _, treeName := range img.manifest.LayerTarPaths {
        print("Layer: ", treeName, "\n")
    }
	// build the content tree
    /*
	for _, treeName := range img.manifest.LayerTarPaths {
		tr, exists := img.layerMap[treeName]
		if exists {
			trees = append(trees, tr)
			continue
		}
        print("Could not find flayer: ", len(img.layerMap), " layers\n")
        for k, v := range img.layerMap {
            print(k,", ", v, "\n")
        }
        debug.PrintStack()

		return nil, fmt.Errorf("could not find '%s' in parsed layers", treeName)
	}
    */

    for _, diffid := range img.config.RootFs.DiffIds {
        trees = append(trees, img.layerMap[diffid])
    }

	// build the layers array
	layers := make([]*image.Layer, 0)

	// note that the engineResolver config stores images in reverse chronological order, so iterate backwards through layers
	// as you iterate chronologically through history (ignoring history items that have no layer contents)
	// Note: history is not required metadata in a docker image!
	histIdx := 0
	for idx, tree := range trees {
		// ignore empty layers, we are only observing layers with content
		historyObj := historyEntry{
			CreatedBy: "(missing)",
		}
		for nextHistIdx := histIdx; nextHistIdx < len(img.config.History); nextHistIdx++ {
			if !img.config.History[nextHistIdx].EmptyLayer {
				histIdx = nextHistIdx
				break
			}
		}
		if histIdx < len(img.config.History) && !img.config.History[histIdx].EmptyLayer {
			historyObj = img.config.History[histIdx]
			histIdx++
		}

		historyObj.Size = tree.FileSize

		dockerLayer := layer{
			history: historyObj,
			index:   idx,
			tree:    tree,
		}
		layers = append(layers, dockerLayer.ToLayer())
	}

	return &image.Image{
		Trees:  trees,
		Layers: layers,
	}, nil
}
