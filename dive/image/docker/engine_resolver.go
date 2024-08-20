package docker

import (
	"fmt"
	"io"
    "encoding/json"
	"bufio"
	"net/http"
	"os"
    "strconv"
	"os/exec"
	"strings"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/client"
	"golang.org/x/net/context"

	"github.com/wagoodman/dive/dive/image"
)

type engineResolver struct{}

func NewResolverFromEngine() *engineResolver {
	return &engineResolver{}
}
/*
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "size": 2175,
    "digest": "sha256:3e9dd60ae426f2f0ec90ffc0220299521e509999187c7313a76522e36d1b3c4f"
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "size": 104182,
      "digest": "sha256:f531499c6b730fc55a63e5ade55ce2c849bbf03f894248e3a2092689e3749312"
    },
*/

type ManifestLayer struct {
    MediaType string `json:"mediaType"`
    Size int64 `json:"size"`
    Digest string `json:"digest"`
}

type OciManifest struct {
    SchemaVersion int64 `json:"schemaVersion"`
    MediaType string `json:"mediaType"`
    Config ManifestLayer `json:"config"`
    Layers []ManifestLayer `json:"layers"`
}

type OciConfig struct {
    manifest OciManifest
    dir string
    raw_manifest []byte
}

func (r *engineResolver) Fetch(id string) (*image.Image, error) {
    print("Fetching image: ", id, "\n")

	oci, err := r.fetchArchive(id)
	if err != nil {
		return nil, err
	}

	img, err := NewImageArchiveFromDir(oci.dir, oci.manifest, oci.raw_manifest)
	if err != nil {
		return nil, err
	}
	return img.ToImage()
}

func (r *engineResolver) Build(args []string) (*image.Image, error) {
	id, err := buildImageFromCli(args)
	if err != nil {
		return nil, err
	}
	return r.Fetch(id)
}

func (r *engineResolver) fetchArchive(id string) (OciConfig, error) {
	var err error
	var dockerClient *client.Client

	// pull the engineResolver if it does not exist
	ctx := context.Background()

	host := os.Getenv("DOCKER_HOST")
	var clientOpts []client.Opt

	switch strings.Split(host, ":")[0] {
	case "ssh":
		helper, err := connhelper.GetConnectionHelper(host)
		if err != nil {
			fmt.Println("docker host", err)
		}
		clientOpts = append(clientOpts, func(c *client.Client) error {
			httpClient := &http.Client{
				Transport: &http.Transport{
					DialContext: helper.Dialer,
				},
			}
			return client.WithHTTPClient(httpClient)(c)
		})
		clientOpts = append(clientOpts, client.WithHost(helper.Host))
		clientOpts = append(clientOpts, client.WithDialContext(helper.Dialer))

	default:

		if os.Getenv("DOCKER_TLS_VERIFY") != "" && os.Getenv("DOCKER_CERT_PATH") == "" {
			os.Setenv("DOCKER_CERT_PATH", "~/.docker")
		}

		clientOpts = append(clientOpts, client.FromEnv)
	}

	clientOpts = append(clientOpts, client.WithAPIVersionNegotiation())
	dockerClient, err = client.NewClientWithOpts(clientOpts...)
	if err != nil {
		return OciConfig{}, err
	}
    inspect, _, err := dockerClient.ImageInspectWithRaw(ctx, id)
	if err != nil {
		// don't use the API, the CLI has more informative output
		fmt.Println("Handler not available locally. Trying to pull '" + id + "'...")
		err = runDockerCmd("pull", id)
		if err != nil {
            return OciConfig{}, err
		}
	}

    print("Image ID: ", id, "\n")
    fmt.Printf("inspect: %+v\n", inspect)

    temp_dir, err := os.MkdirTemp("", "dive-registry-")
    print("Temp dir: ", temp_dir, "\n")
    if err != nil {
        print("Error creating temp dir: ", err, "\n")
    }
    cmd := exec.Command("/home/tbirch/src/dive/crane", "registry", "serve", "--insecure", "--disk", temp_dir)
    stderrReader, stderrWriter := io.Pipe()
    cmd.Stderr = stderrWriter
    print("Running command: ", cmd, "\n")
    cmd.Start()
    print("Command started: ", cmd, "\n")
    ch := make(chan int)

    in := bufio.NewReader(stderrReader)

    go func() {
        for true {
            b, _, _ := in.ReadLine()
            s := string(b)
            if strings.Contains(s, "serving on port") {
                splits := strings.Split(s, " ")
                port := splits[len(splits) - 1]
                k, _ := strconv.Atoi(port)
                ch <- k
                break;
            }
        }
        for true {
            in.ReadLine()
        }
    }()

    port := <-ch

    imageTag := "localhost:" + strconv.Itoa(port) + "/image:latest"
    runDockerCmd("tag", id, imageTag)
    runDockerCmd("push", imageTag)

    resp, _ := http.Get("http://localhost:" + strconv.Itoa(port) + "/v2/image/manifests/latest")
    body, _ := io.ReadAll(resp.Body)
    resp.Body.Close()


    var manifest OciManifest
    err = json.Unmarshal(body, &manifest)
    if err != nil {
        fmt.Println("ManifestErr:", err)
    }

    cmd.Process.Kill()

    print("Done fetching image: ", id, "\n")

    return OciConfig{manifest, temp_dir + "/sha256/", body}, nil
}
