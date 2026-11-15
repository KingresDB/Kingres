// SPDX-License-Identifier: AGPL-3.0-only

// Package rowfile reads the description of a row that kingres-verify
// row-hash takes: a JSON array with one object per non-NULL column, in column
// order, each with the column name and the hex of the value's binary send
// form.
package rowfile

import (
	"fmt"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/jsonx"
)

// Parse parses a row description:
//
//	[{"name": "id", "send_hex": "00000001"}, {"name": "note", "send_hex": ""}]
//
// name is the column name (UTF-8); send_hex is the lower case hex of the bytes
// the column type's binary send function produces (empty for a zero length
// value). NULL columns are left out. Unknown members are rejected, because a
// misspelled member would otherwise change the hash silently.
func Parse(data []byte) ([]format.Column, error) {
	v, err := jsonx.Parse(data)
	if err != nil {
		return nil, err
	}
	items, err := v.Array()
	if err != nil {
		return nil, err
	}
	cols := make([]format.Column, 0, len(items))
	for _, it := range items {
		o, err := it.Object()
		if err != nil {
			return nil, err
		}
		if err := o.OnlyKeys("name", "send_hex"); err != nil {
			return nil, err
		}
		name, err := o.String("name")
		if err != nil {
			return nil, err
		}
		hx, err := o.String("send_hex")
		if err != nil {
			return nil, err
		}
		val, err := format.DecodeHex(hx)
		if err != nil {
			return nil, fmt.Errorf("%s.send_hex: %w", o.Path(), err)
		}
		cols = append(cols, format.Column{Name: name, Value: val})
	}
	return cols, nil
}
