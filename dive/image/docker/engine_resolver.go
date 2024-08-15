package docker

import (
	"fmt"
	"io"
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

func (r *engineResolver) Fetch(id string) (*image.Image, error) {
    print("Fetching image: ", id, "\n")

    temp_dir, err := os.MkdirTemp("", "dive-registry-")
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
    print("Port: ", port, "\n")

    cmd.Process.Kill()

    print("Done fetching image: ", id, "\n")

	reader, err := r.fetchArchive(id)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	img, err := NewImageArchive(reader)
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

func (r *engineResolver) fetchArchive(id string) (io.ReadCloser, error) {
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
		return nil, err
	}
    inspect, _, err := dockerClient.ImageInspectWithRaw(ctx, id)
	if err != nil {
		// don't use the API, the CLI has more informative output
		fmt.Println("Handler not available locally. Trying to pull '" + id + "'...")
		err = runDockerCmd("pull", id)
		if err != nil {
			return nil, err
		}
	}

    print("Image ID: ", id, "\n")
    fmt.Printf("inspect: %+v\n", inspect)

	readCloser, err := dockerClient.ImageSave(ctx, []string{id})
	if err != nil {
		return nil, err
	}

	return readCloser, nil
}
