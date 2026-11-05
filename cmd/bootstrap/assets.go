package main

import (
	"embed"
)

//go:embed crds/*.yaml
var crdFS embed.FS

//go:embed manifests/infrastructure-components.yaml
var infraComponentsYAML []byte
