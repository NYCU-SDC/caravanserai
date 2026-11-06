// Package schemas embeds the generated JSON Schemas, so caractl explain can
// read them without a server or a checkout. `make schemas` writes the files;
// this package only carries them.
package schemas

import "embed"

// FS holds node.json, project.json and secret.json.
//
//go:embed *.json
var FS embed.FS
