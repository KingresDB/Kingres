// SPDX-License-Identifier: AGPL-3.0-only

// Package logsjson reads logs.json, the list of the logs of a publication (ledger-phase7.md 4.6, correction 178):
//
//	{"format":1,"logs":[{"kind":"chain","origin":"<origin>","state":"active","size":3},...]}
//
// It is written by the publisher, in the order the logs were made, and it is an index like bundle.json: the
// list says where to look and is not believed. It is read strictly (duplicate keys, an unknown member, an
// unknown format, kind or state are refused, not guessed), because the origins of it name files that
// bundle fetch asks a server for and strings of it are shown to the auditor.
package logsjson

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/jsonx"
)

// Limits.
const (
	MaxSize    = 1 << 20
	MaxEntries = 4096
)

// Log is one entry of the list.
type Log struct {
	Kind   string // "chain" or "aggregate"
	Origin string
	State  string // "active", "diverged" or "closed"
	Size   uint64
	// ID is the 64 hex characters of the chain id or of the aggregate id, and Epoch the epoch of a chain log, read from the
	// last elements of the origin (the layout of design 4.2). Prefix is what comes before them.
	ID     string
	Epoch  uint64
	Prefix string
}

var (
	chainOrigin = regexp.MustCompile(`^(.+)/c/([0-9a-f]{64})/e([1-9][0-9]{0,8})$`)
	aggOrigin   = regexp.MustCompile(`^(.+)/a/([0-9a-f]{64})$`)
)

// Parse reads the file.
func Parse(data []byte) ([]Log, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("logs.json has %d bytes, expected at most %d", len(data), MaxSize)
	}
	v, err := jsonx.Parse(data)
	if err != nil {
		return nil, err
	}
	o, err := v.Object()
	if err != nil {
		return nil, err
	}
	if err := o.OnlyKeys("format", "logs"); err != nil {
		return nil, err
	}
	f, err := o.Int64("format", -1<<62, 1<<62)
	if err != nil {
		return nil, err
	}
	if f != 1 {
		return nil, fmt.Errorf("logs.json has format %d and this program reads format 1 only: it does not guess what another format means", f)
	}
	arr, err := o.Array("logs")
	if err != nil {
		return nil, err
	}
	if len(arr) > MaxEntries {
		return nil, fmt.Errorf("%s.logs has %d entries, expected at most %d", o.Path(), len(arr), MaxEntries)
	}
	out := make([]Log, 0, len(arr))
	seen := map[string]bool{}
	for _, lv := range arr {
		lo, err := lv.Object()
		if err != nil {
			return nil, err
		}
		if err := lo.OnlyKeys("kind", "origin", "state", "size"); err != nil {
			return nil, err
		}
		var l Log
		if l.Kind, err = lo.String("kind"); err != nil {
			return nil, err
		}
		if l.Origin, err = lo.String("origin"); err != nil {
			return nil, err
		}
		if l.State, err = lo.String("state"); err != nil {
			return nil, err
		}
		switch l.State {
		case "active", "diverged", "closed":
		default:
			return nil, fmt.Errorf("%s.state is %q, expected active, diverged or closed", lo.Path(), l.State)
		}
		n, err := lo.Int64("size", 0, 1<<62-1)
		if err != nil {
			return nil, err
		}
		l.Size = uint64(n) // #nosec G115 -- range checked
		switch l.Kind {
		case "chain":
			m := chainOrigin.FindStringSubmatch(l.Origin)
			if m == nil {
				return nil, fmt.Errorf("%s.origin is not <prefix>/c/<64 hex>/e<N>", lo.Path())
			}
			l.Prefix, l.ID = m[1], m[2]
			l.Epoch, _ = strconv.ParseUint(m[3], 10, 64)
		case "aggregate":
			m := aggOrigin.FindStringSubmatch(l.Origin)
			if m == nil {
				return nil, fmt.Errorf("%s.origin is not <prefix>/a/<64 hex>", lo.Path())
			}
			l.Prefix, l.ID = m[1], m[2]
		default:
			return nil, fmt.Errorf("%s.kind is %q, expected chain or aggregate", lo.Path(), l.Kind)
		}
		if seen[l.Origin] {
			return nil, fmt.Errorf("%s: the origin %q is listed twice", lo.Path(), l.Origin)
		}
		seen[l.Origin] = true
		out = append(out, l)
	}
	return out, nil
}
