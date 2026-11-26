// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package bundleidx reads bundle.json, the index of a bundle (ledger-phase7.md 7.3). The index
// is a convenience and is not trusted: nothing in it is believed, every fact is re-derived from the
// signed notes and the tiles. It is still read strictly (duplicate keys, unknown members and any
// path that is not a plain relative path are refused) because a path of it is opened and a
// string of it is shown to the auditor.
package bundleidx

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/jsonx"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

// Limits.
const (
	MaxSize      = 1 << 20
	MaxEntries   = 4096
	MaxNotes     = 500
	MaxPathBytes = 255
)

// Log is a log the index names.
type Log struct {
	Kind       string // "chain" or "aggregate"
	Origin     string
	ChainID    string // chain only, 64 hex
	Epoch      uint64 // chain only
	Checkpoint string
	Size       uint64
}

// Proof is a proof the index names, and the files that go with it.
type Proof struct {
	File               string
	AttestationFile    string
	AttestationColumns string
}

// Index is a parsed bundle.json.
type Index struct {
	CreatedAt    time.Time
	ClusterID    string
	OriginPrefix string
	Logs         []Log
	Anchors      []string
	Proofs       []Proof
	Notes        string
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{1,128}$`)

// Parse reads the index.
func Parse(data []byte) (*Index, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("bundle.json has %d bytes, expected at most %d", len(data), MaxSize)
	}
	v, err := jsonx.Parse(data)
	if err != nil {
		return nil, err
	}
	o, err := v.Object()
	if err != nil {
		return nil, err
	}
	if err := o.OnlyKeys("format", "created_at", "cluster_id", "origin_prefix", "logs", "anchors", "proofs", "notes"); err != nil {
		return nil, err
	}
	if _, err := o.Int64("format", 1, 1); err != nil {
		return nil, err
	}
	ix := &Index{}
	s, err := o.String("created_at")
	if err != nil {
		return nil, err
	}
	if ix.CreatedAt, err = time.Parse(time.RFC3339, s); err != nil {
		return nil, fmt.Errorf("%s.created_at: %q is not an RFC 3339 time", o.Path(), s)
	}
	if ix.ClusterID, err = o.String("cluster_id"); err != nil {
		return nil, err
	}
	if !hexRE.MatchString(ix.ClusterID) {
		return nil, fmt.Errorf("%s.cluster_id must be lower case hex", o.Path())
	}
	if ix.OriginPrefix, err = o.String("origin_prefix"); err != nil {
		return nil, err
	}
	if o.Has("notes") {
		if ix.Notes, err = o.String("notes"); err != nil {
			return nil, err
		}
		if len([]rune(ix.Notes)) > MaxNotes {
			return nil, fmt.Errorf("%s.notes has more than %d characters", o.Path(), MaxNotes)
		}
	}
	logs, err := o.Array("logs")
	if err != nil {
		return nil, err
	}
	if len(logs) > MaxEntries {
		return nil, fmt.Errorf("%s.logs has %d entries, expected at most %d", o.Path(), len(logs), MaxEntries)
	}
	for _, lv := range logs {
		l, err := parseLog(lv)
		if err != nil {
			return nil, err
		}
		ix.Logs = append(ix.Logs, l)
	}
	if o.Has("anchors") {
		as, err := o.Array("anchors")
		if err != nil {
			return nil, err
		}
		if len(as) > MaxEntries {
			return nil, fmt.Errorf("%s.anchors has %d entries, expected at most %d", o.Path(), len(as), MaxEntries)
		}
		for _, av := range as {
			p, err := av.String()
			if err != nil {
				return nil, err
			}
			if err := CheckPath(p); err != nil {
				return nil, fmt.Errorf("%s: %w", av.Path(), err)
			}
			ix.Anchors = append(ix.Anchors, p)
		}
	}
	if o.Has("proofs") {
		ps, err := o.Array("proofs")
		if err != nil {
			return nil, err
		}
		if len(ps) > MaxEntries {
			return nil, fmt.Errorf("%s.proofs has %d entries, expected at most %d", o.Path(), len(ps), MaxEntries)
		}
		for _, pv := range ps {
			po, err := pv.Object()
			if err != nil {
				return nil, err
			}
			if err := po.OnlyKeys("file", "attestation_file", "attestation_columns"); err != nil {
				return nil, err
			}
			var pr Proof
			for _, f := range []struct {
				name string
				dst  *string
				need bool
			}{{"file", &pr.File, true}, {"attestation_file", &pr.AttestationFile, false}, {"attestation_columns", &pr.AttestationColumns, false}} {
				if !po.Has(f.name) {
					if f.need {
						return nil, fmt.Errorf("%s has no member %q", po.Path(), f.name)
					}
					continue
				}
				if *f.dst, err = po.String(f.name); err != nil {
					return nil, err
				}
				if err := CheckPath(*f.dst); err != nil {
					return nil, fmt.Errorf("%s.%s: %w", po.Path(), f.name, err)
				}
			}
			if (pr.AttestationFile == "") != (pr.AttestationColumns == "") {
				return nil, fmt.Errorf("%s: attestation_file and attestation_columns go together", po.Path())
			}
			ix.Proofs = append(ix.Proofs, pr)
		}
	}
	return ix, nil
}

var (
	chainIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func parseLog(v jsonx.Value) (Log, error) {
	o, err := v.Object()
	if err != nil {
		return Log{}, err
	}
	if err := o.OnlyKeys("kind", "origin", "chain_id", "epoch", "checkpoint", "size"); err != nil {
		return Log{}, err
	}
	var l Log
	if l.Kind, err = o.String("kind"); err != nil {
		return Log{}, err
	}
	switch l.Kind {
	case "chain":
		if l.ChainID, err = o.String("chain_id"); err != nil {
			return Log{}, err
		}
		if !chainIDRE.MatchString(l.ChainID) {
			return Log{}, fmt.Errorf("%s.chain_id must be 64 lower case hex characters", o.Path())
		}
		e, err := o.Int64("epoch", 1, 1<<31-1)
		if err != nil {
			return Log{}, err
		}
		l.Epoch = uint64(e) // #nosec G115 -- range checked
	case "aggregate":
		if o.Has("chain_id") || o.Has("epoch") {
			return Log{}, fmt.Errorf("%s: an aggregate log has no chain_id and no epoch", o.Path())
		}
	default:
		return Log{}, fmt.Errorf("%s.kind is %q, expected chain or aggregate", o.Path(), l.Kind)
	}
	if l.Origin, err = o.String("origin"); err != nil {
		return Log{}, err
	}
	if l.Checkpoint, err = o.String("checkpoint"); err != nil {
		return Log{}, err
	}
	if err := CheckPath(l.Checkpoint); err != nil {
		return Log{}, fmt.Errorf("%s.checkpoint: %w", o.Path(), err)
	}
	n, err := o.Int64("size", 0, 1<<62-1)
	if err != nil {
		return Log{}, err
	}
	l.Size = uint64(n) // #nosec G115 -- range checked
	return l, nil
}

// CheckPath refuses a path that is not a plain relative path inside the bundle: empty, absolute, with a
// dot or dot-dot element, a backslash, a colon, a NUL or a control character, over 255 bytes, or with
// more than 16 elements.
func CheckPath(p string) error {
	if p == "" {
		return errors.New("the path is empty")
	}
	if len(p) > MaxPathBytes {
		return fmt.Errorf("the path has %d bytes, expected at most %d", len(p), MaxPathBytes)
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return errors.New("the path has a control character")
		}
	}
	if strings.HasPrefix(p, "/") {
		return errors.New("the path is absolute: a bundle names files inside itself")
	}
	if _, err := safefile.CleanRel(p); err != nil {
		return err
	}
	return nil
}
