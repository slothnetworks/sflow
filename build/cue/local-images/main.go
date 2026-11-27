package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

type sliceFlag []string

func (i *sliceFlag) String() string {
	return fmt.Sprintf("%v", *i)
}

func (i *sliceFlag) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func main() {
	var images sliceFlag
	var manifests sliceFlag

	flag.Var(&images, "image", "")
	flag.Var(&manifests, "manifest", "")
	flag.Parse()

	out, err := os.OpenFile(flag.Arg(0), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()

	_, _ = fmt.Fprintf(out, "package k8s\n")
	for i := 0; i < len(images); i++ {
		m, err := readManifest(manifests[i])
		if err != nil {
			log.Fatal(err)
		}

		if len(m.RepoTags) != 1 {
			log.Fatalf("unexpected repotags count: %d", len(m.RepoTags))
		}

		digest := strings.TrimPrefix(m.Config, "blobs/")
		digest = strings.ReplaceAll(digest, "/", ":")
		imageName := images[i]
		tag := m.RepoTags[0]

		_, _ = fmt.Fprintf(out, "netmeta: images: %s: {image: %q, digest: %q}\n", imageName, tag, digest)
	}
}

func readManifest(name string) (*Manifest, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	defer f.Close()

	var manifests []Manifest
	if err := json.NewDecoder(f).Decode(&manifests); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if len(manifests) != 1 {
		return nil, fmt.Errorf("unexpected manifest count: %d", len(manifests))
	}

	return &manifests[0], nil
}

type Manifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}
