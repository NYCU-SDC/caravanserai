package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"NYCU-SDC/caravanserai/schemas"

	"github.com/spf13/cobra"
)

// NewExplainCmd returns the "explain" subcommand.
func NewExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain <kind>[.<field path>]",
		Short: "Describe a resource's fields from the built-in schema",
		Long: `explain prints a field's type, description and child fields, read from the
JSON Schemas embedded in caractl at build time. It needs no server.

Kinds: node, project, secret.

Examples:
  caractl explain project
  caractl explain project.spec.services
  caractl explain project.spec.services.env -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			outputFmt, _ := cmd.Root().PersistentFlags().GetString("output")
			cmd.SilenceUsage = true
			ex, err := explain(args[0])
			if err != nil {
				return err
			}
			switch outputFmt {
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(ex)
			case "table", "":
				ex.write(cmd.OutOrStdout())
				return nil
			}
			return fmt.Errorf("explain supports -o table or json, not %q", outputFmt)
		},
	}
}

// schemaNode is the part of a JSON Schema (as cmd/schemagen writes it) that
// explain reads.
type schemaNode struct {
	Ref                  string                 `json:"$ref"`
	Type                 string                 `json:"type"`
	Format               string                 `json:"format"`
	Description          string                 `json:"description"`
	Enum                 []any                  `json:"enum"`
	Required             []string               `json:"required"`
	Properties           map[string]*schemaNode `json:"properties"`
	Items                *schemaNode            `json:"items"`
	AdditionalProperties json.RawMessage        `json:"additionalProperties"`
	Defs                 map[string]*schemaNode `json:"$defs"`
}

// schemaKinds maps what a user may type to the embedded file and the kind's
// name.
var schemaKinds = map[string][2]string{
	"node": {"node.json", "Node"}, "nodes": {"node.json", "Node"},
	"project": {"project.json", "Project"}, "projects": {"project.json", "Project"},
	"secret": {"secret.json", "Secret"}, "secrets": {"secret.json", "Secret"},
}

// Explanation is what explain prints; -o json prints it as is.
type Explanation struct {
	Kind        string           `json:"kind"`
	Path        string           `json:"path,omitempty"`
	Type        string           `json:"type"`
	Description string           `json:"description,omitempty"`
	Enum        []any            `json:"enum,omitempty"`
	Fields      []ExplainedField `json:"fields,omitempty"`
}

// ExplainedField is one child field.
type ExplainedField struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

func explain(arg string) (*Explanation, error) {
	parts := strings.Split(arg, ".")
	kindArg, path := strings.ToLower(parts[0]), parts[1:]
	k, ok := schemaKinds[kindArg]
	if !ok {
		return nil, fmt.Errorf("unknown kind %q: valid kinds are node, project, secret", parts[0])
	}
	raw, err := schemas.FS.ReadFile(k[0])
	if err != nil {
		return nil, fmt.Errorf("read embedded schema: %w", err)
	}
	var root schemaNode
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse embedded schema %s: %w", k[0], err)
	}
	s := schemaDoc{defs: root.Defs}

	field := &root
	walked := []string{}
	for _, name := range path {
		parent := s.object(field)
		child, ok := parent.Properties[name]
		if !ok {
			where := k[1]
			if len(walked) > 0 {
				where += "." + strings.Join(walked, ".")
			}
			children := sortedKeys(parent.Properties)
			if len(children) == 0 {
				return nil, fmt.Errorf("field %q does not exist: %s has no child fields", name, where)
			}
			return nil, fmt.Errorf("field %q does not exist in %s; valid fields: %s", name, where, strings.Join(children, ", "))
		}
		field = child
		walked = append(walked, name)
	}

	resolved := s.resolve(field)
	ex := &Explanation{
		Kind:        k[1],
		Path:        strings.Join(walked, "."),
		Type:        s.typeName(field),
		Description: firstNonEmpty(field.Description, resolved.Description),
		Enum:        resolved.Enum,
	}
	obj := s.object(field)
	required := map[string]bool{}
	for _, r := range obj.Required {
		required[r] = true
	}
	for _, name := range sortedKeys(obj.Properties) {
		child := obj.Properties[name]
		ex.Fields = append(ex.Fields, ExplainedField{
			Name:        name,
			Type:        s.typeName(child),
			Required:    required[name],
			Description: firstNonEmpty(child.Description, s.resolve(child).Description),
		})
	}
	return ex, nil
}

type schemaDoc struct {
	defs map[string]*schemaNode
}

// resolve follows $ref.
func (s schemaDoc) resolve(n *schemaNode) *schemaNode {
	for n.Ref != "" {
		def, ok := s.defs[strings.TrimPrefix(n.Ref, "#/$defs/")]
		if !ok {
			return n
		}
		n = def
	}
	return n
}

// object resolves n and steps through arrays to the element type, which is
// where the child fields of a list field live.
func (s schemaDoc) object(n *schemaNode) *schemaNode {
	n = s.resolve(n)
	for n.Type == "array" && n.Items != nil {
		n = s.resolve(n.Items)
	}
	return n
}

// typeName writes a field's type the way Go reads: ServiceDef, []EnvVar,
// map[string]string, string (date-time).
func (s schemaDoc) typeName(n *schemaNode) string {
	if n.Ref != "" {
		name := strings.TrimPrefix(n.Ref, "#/$defs/")
		def := s.resolve(n)
		if (def.Type == "object" && len(def.Properties) > 0) || def.Type == "" {
			return name
		}
		return s.typeName(def) + " (" + name + ")"
	}
	switch n.Type {
	case "array":
		if n.Items != nil {
			return "[]" + s.typeName(n.Items)
		}
		return "[]any"
	case "object":
		var elem schemaNode
		if len(n.AdditionalProperties) > 0 && json.Unmarshal(n.AdditionalProperties, &elem) == nil && (elem.Type != "" || elem.Ref != "") {
			return "map[string]" + s.typeName(&elem)
		}
		return "object"
	case "":
		return "any"
	}
	if n.Format != "" {
		return n.Type + " (" + n.Format + ")"
	}
	return n.Type
}

func (ex *Explanation) write(w io.Writer) {
	fmt.Fprintf(w, "KIND:  %s\n", ex.Kind)
	if ex.Path != "" {
		fmt.Fprintf(w, "FIELD: %s <%s>\n", ex.Path, ex.Type)
	}
	if ex.Description != "" {
		fmt.Fprintf(w, "\nDESCRIPTION:\n%s\n", indent(ex.Description, "  "))
	}
	if len(ex.Enum) > 0 {
		vals := make([]string, len(ex.Enum))
		for i, v := range ex.Enum {
			vals[i] = fmt.Sprint(v)
		}
		fmt.Fprintf(w, "\nVALUES:\n  %s\n", strings.Join(vals, ", "))
	}
	if len(ex.Fields) > 0 {
		fmt.Fprintf(w, "\nFIELDS:\n")
		for _, f := range ex.Fields {
			req := ""
			if f.Required {
				req = " (required)"
			}
			fmt.Fprintf(w, "  %s <%s>%s\n", f.Name, f.Type, req)
			if f.Description != "" {
				fmt.Fprintf(w, "%s\n", indent(f.Description, "    "))
			}
		}
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func sortedKeys(m map[string]*schemaNode) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
