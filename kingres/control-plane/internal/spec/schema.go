// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package spec

import (
	"encoding/json"
	"fmt"
	"strings"
)

// JSON returns the document as JSON, the form that JSON Schema validators
// load.
func (d *Document) JSON() ([]byte, error) {
	b, err := json.Marshal(d.Raw)
	if err != nil {
		return nil, fmt.Errorf("encoding OpenAPI document as JSON: %w", err)
	}
	return b, nil
}

// escapePointer escapes a JSON Pointer token (RFC 6901).
func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// ResponseSchemaPointer returns the JSON Pointer fragment (starting with "#")
// of the schema of the application/json response with the given status of an
// operation, and false if the operation has no inline JSON response for that
// status (for example because the response is a $ref to components/responses).
func (d *Document) ResponseSchemaPointer(opID, status string) (string, bool) {
	for _, op := range d.Operations {
		if op.ID != opID {
			continue
		}
		method := strings.ToLower(op.Method)
		ptr := "#/paths/" + escapePointer(op.Path) + "/" + method + "/responses/" + status + "/content/application~1json/schema"
		cur := any(d.Raw)
		for _, tok := range strings.Split(ptr[2:], "/") {
			tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
			m := asMap(cur)
			next, ok := m[tok]
			if !ok {
				return "", false
			}
			cur = next
		}
		return ptr, true
	}
	return "", false
}
