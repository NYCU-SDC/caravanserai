// schemagen generates JSON Schema (Draft 2020-12) files from Caravanserai's
// Go API types. Run via: make schemas
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/invopop/jsonschema"
)

// resource pairs a Go type with its output filename and display title.
type resource struct {
	instance any
	filename string
	title    string
}

func main() {
	resources := []resource{
		{instance: &v1.Node{}, filename: "node.json", title: "Node"},
		{instance: &v1.Project{}, filename: "project.json", title: "Project"},
	}

	r := new(jsonschema.Reflector)
	if err := r.AddGoComments("NYCU-SDC/caravanserai", "./"); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: add go comments: %v\n", err)
		os.Exit(1)
	}

	outDir := "schemas"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: create output dir: %v\n", err)
		os.Exit(1)
	}

	for _, res := range resources {
		schema := r.Reflect(res.instance)
		schema.ID = jsonschema.ID(res.filename)
		schema.Title = res.title

		data, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "schemagen: marshal %s: %v\n", res.filename, err)
			os.Exit(1)
		}

		path := filepath.Join(outDir, res.filename)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "schemagen: write %s: %v\n", path, err)
			os.Exit(1)
		}

		fmt.Printf("wrote %s\n", path)
	}
}
