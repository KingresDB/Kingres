// SPDX-License-Identifier: PostgreSQL

// Package spec loads the OpenAPI 3.1 contract (kingres/api/openapi.yaml) and
// extracts what the server needs from it: the list of operations with their
// required role and security schemes, and the JSON Schemas of the components.
//
// It is used by the code generator in internal/contract/gen and by tests. The
// kingresd binary itself does not import it: the operation table is
// generated at build time, so no YAML parser is linked into the server.
package spec

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Operation describes one operation of the contract.
type Operation struct {
	// ID is the operationId.
	ID string
	// Method is the upper-case HTTP method.
	Method string
	// Path is the path template relative to the base path, for example
	// "/servers/{serverId}".
	Path string
	// Tag is the first tag of the operation.
	Tag string
	// Role is the value of x-required-role.
	Role string
	// Security lists the names of the security schemes of which any one
	// authenticates the request. It is empty for a public operation.
	Security []string
	// PathParams are the names of the path parameters in order of appearance.
	PathParams []string
	// QueryParams are the names of the declared query parameters, sorted.
	QueryParams []string
	// Responses are the declared response status codes, sorted.
	Responses []string
}

// Document is a parsed OpenAPI document.
type Document struct {
	// Raw is the whole document converted to JSON-compatible values
	// (map[string]any, []any, string, float64, int, bool, nil).
	Raw map[string]any
	// Operations lists every operation in the order of the document.
	Operations []Operation
	// AuditActions is the enum of schema AuditAction.
	AuditActions []string
}

var validRoles = map[string]bool{
	"none": true, "kingres_stats": true, "kingres_readonly": true,
	"kingres_ops": true, "kingres_admin": true,
}

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// Load reads and parses the OpenAPI file at path.
func Load(path string) (*Document, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is a command-line argument of a build tool
	if err != nil {
		return nil, fmt.Errorf("reading OpenAPI file: %w", err)
	}
	return Parse(data)
}

// Parse parses an OpenAPI 3.1 document in YAML or JSON form.
func Parse(data []byte) (*Document, error) {
	var root any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing OpenAPI YAML: %w", err)
	}
	norm, ok := normalize(root).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("OpenAPI document is not a mapping")
	}
	doc := &Document{Raw: norm}
	if err := doc.extractOperations(); err != nil {
		return nil, err
	}
	doc.AuditActions = doc.EnumOf("AuditAction")
	return doc, nil
}

// normalize converts yaml.v3 output into JSON-compatible values: all maps
// become map[string]any.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normalize(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[fmt.Sprint(k)] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalize(e)
		}
		return out
	default:
		return v
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// resolveRef follows a local "$ref" such as "#/components/parameters/Limit".
func (d *Document) resolveRef(v any) (any, error) {
	for i := 0; i < 16; i++ {
		m := asMap(v)
		ref, ok := m["$ref"].(string)
		if !ok {
			return v, nil
		}
		if !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("unsupported non-local $ref %q", ref)
		}
		var cur any = d.Raw
		for _, part := range strings.Split(ref[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			cm := asMap(cur)
			next, found := cm[part]
			if !found {
				return nil, fmt.Errorf("unresolved $ref %q", ref)
			}
			cur = next
		}
		v = cur
	}
	return nil, fmt.Errorf("$ref chain too deep")
}

func (d *Document) extractOperations() error {
	// The order of paths in the file is not preserved by a map, so recover it
	// from the sorted keys for a deterministic result.
	paths := asMap(d.Raw["paths"])
	if paths == nil {
		return fmt.Errorf("OpenAPI document has no paths")
	}
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	defaultSecurity := securityNames(d.Raw["security"])
	seen := map[string]bool{}
	for _, p := range keys {
		item := asMap(paths[p])
		shared := asSlice(item["parameters"])
		for _, m := range methods {
			opv, ok := item[m]
			if !ok {
				continue
			}
			op := asMap(opv)
			id, _ := op["operationId"].(string)
			if id == "" {
				return fmt.Errorf("%s %s has no operationId", strings.ToUpper(m), p)
			}
			if seen[id] {
				return fmt.Errorf("duplicate operationId %q", id)
			}
			seen[id] = true
			role, _ := op["x-required-role"].(string)
			if !validRoles[role] {
				return fmt.Errorf("operation %s: invalid or missing x-required-role %q", id, role)
			}
			o := Operation{ID: id, Method: strings.ToUpper(m), Path: p, Role: role}
			if tags := asSlice(op["tags"]); len(tags) > 0 {
				o.Tag, _ = tags[0].(string)
			}
			if sec, has := op["security"]; has {
				o.Security = securityNames(sec)
			} else {
				o.Security = defaultSecurity
			}
			params := append(append([]any{}, shared...), asSlice(op["parameters"])...)
			for _, pv := range params {
				rv, err := d.resolveRef(pv)
				if err != nil {
					return fmt.Errorf("operation %s: %w", id, err)
				}
				pm := asMap(rv)
				name, _ := pm["name"].(string)
				switch pm["in"] {
				case "query":
					o.QueryParams = append(o.QueryParams, name)
				case "path":
					o.PathParams = append(o.PathParams, name)
				}
			}
			sort.Strings(o.QueryParams)
			for code := range asMap(op["responses"]) {
				o.Responses = append(o.Responses, code)
			}
			sort.Strings(o.Responses)
			d.Operations = append(d.Operations, o)
		}
	}
	return nil
}

// EnumOf returns the enum values of a string component schema.
func (d *Document) EnumOf(schema string) []string {
	comp := asMap(asMap(d.Raw["components"])["schemas"])
	var out []string
	for _, v := range asSlice(asMap(comp[schema])["enum"]) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// securityNames flattens a security requirement list into scheme names.
func securityNames(v any) []string {
	var out []string
	for _, req := range asSlice(v) {
		for name := range asMap(req) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// SchemaPointer returns the JSON pointer of a component schema.
func SchemaPointer(name string) string {
	return "#/components/schemas/" + name
}
