package docker

import (
	"encoding/json"
    "fmt"

	"github.com/sirupsen/logrus"
)

type manifest struct {
	ConfigPath    string   `json:"Config"`
	RepoTags      []string `json:"RepoTags"`
	LayerTarPaths []string `json:"Layers"`
}

func newManifest(manifestBytes []byte) manifest {
	var manifest []manifest
    fmt.Println("manifestBytes: ", string(manifestBytes))
	err := json.Unmarshal(manifestBytes, &manifest)
	if err != nil {
		logrus.Panic(err)
	}
	return manifest[0]
}
