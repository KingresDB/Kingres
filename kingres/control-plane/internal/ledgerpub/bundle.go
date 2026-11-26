// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerpub

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
)

// The bundle (design 4.6 and 7.3): a .tar.gz of the layout of the publication, limited to the logs of one database, with one more file at the
// top, bundle.json, a convenience index that a verifier does not trust (every fact is derived again from the signed notes and the tiles).

// Errors of WriteBundle.
var (
	// ErrNoLogs: the database has no published log, or none of the origins asked for is one of its logs.
	ErrNoLogs = errors.New("ledgerpub: the database has no published log for the bundle")
	// ErrUnknownOrigin: an origin of the request is not a log of the database.
	ErrUnknownOrigin = errors.New("ledgerpub: an origin of the request is not a log of the database")
)

// BundleRequest is what a bundle holds.
type BundleRequest struct {
	// Logs are origins; empty means every log of the database.
	Logs []string
	// SinceSize leaves out the entry bundles that lie wholly below this size of a log; the tiles of hashes stay (a proof needs them).
	SinceSize int64
	// IncludeAnchors adds the evidence file of every anchored aggregate checkpoint.
	IncludeAnchors bool
	// Files are further files of the request, by path in the bundle (the inclusion proofs the caller made).
	Files map[string][]byte
	// Notes is free text of at most 500 characters.
	Notes string
}

var bundleLogPathRE = regexp.MustCompile(`/((?:c/[0-9a-f]{64}/e[0-9]+)|(?:a/[0-9a-f]{64}))$`)

type bundleLog struct {
	Kind       string `json:"kind"`
	Origin     string `json:"origin"`
	ChainID    string `json:"chain_id,omitempty"`
	Epoch      int    `json:"epoch,omitempty"`
	Checkpoint string `json:"checkpoint"`
	Size       int64  `json:"size"`
}

type bundleIndex struct {
	Format       int           `json:"format"`
	CreatedAt    string        `json:"created_at"`
	ClusterID    string        `json:"cluster_id"`
	OriginPrefix string        `json:"origin_prefix"`
	Logs         []bundleLog   `json:"logs"`
	Anchors      []string      `json:"anchors"`
	Proofs       []bundleProof `json:"proofs,omitempty"`
	Notes        string        `json:"notes,omitempty"`
}

// BundleInfo says what a bundle holds.
type BundleInfo struct {
	Logs, Files int
	Bytes       int64
}

// WriteBundle writes the bundle of a database to w. A log with no checkpoint yet is left out. The files are read from the store one by one,
// so the memory used is that of the largest tile, not of the bundle.
func (s PGStore) WriteBundle(ctx context.Context, db string, req BundleRequest, prefix string, now time.Time, w io.Writer) (BundleInfo, error) {
	var info BundleInfo
	if len(req.Notes) > 500 {
		return info, errors.New("ledgerpub: the notes of a bundle are at most 500 characters")
	}
	logs, err := s.Logs(ctx)
	if err != nil {
		return info, err
	}
	var mine []LogRow
	for _, l := range logs {
		if l.Database == db && l.Size > 0 {
			mine = append(mine, l)
		}
	}
	want := map[string]bool{}
	for _, o := range req.Logs {
		want[o] = true
	}
	var chosen []LogRow
	for _, l := range mine {
		if len(want) == 0 || want[l.Origin] {
			chosen = append(chosen, l)
		}
		delete(want, l.Origin)
	}
	if len(want) > 0 {
		return info, ErrUnknownOrigin
	}
	if len(chosen) == 0 {
		return info, ErrNoLogs
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: now.UTC(), Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		info.Files++
		info.Bytes += int64(len(data))
		return err
	}
	root := func(rest string) error {
		o, err := s.Object(ctx, "", rest)
		if err != nil {
			return fmt.Errorf("%s: %w", rest, err)
		}
		return put(rest, o.Data)
	}
	if err := root("keyring.json"); err != nil && !errors.Is(err, ErrNotFound) {
		return info, err
	}
	var events int64
	if err := s.Sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT coalesce(max(seq) + 1, 0) FROM kingres_cp.ledger_event`).Scan(&events)
	}); err != nil {
		return info, err
	}
	for n := int64(0); n < events; n++ {
		if err := root(fmt.Sprintf("events/%06d", n)); err != nil {
			if errors.Is(err, ErrNotFound) {
				// the event files are numbered without padding by the store: read them by the number
				if err := root(fmt.Sprintf("events/%d", n)); err != nil {
					return info, err
				}
			} else {
				return info, err
			}
		}
	}
	var cid string
	if err := s.Sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT coalesce(encode(cluster_id, 'hex'), '') FROM kingres_cp.ledger_cluster`).Scan(&cid)
	}); err != nil {
		return info, err
	}
	if cid == "" {
		return info, ErrNoLogs
	}
	idx := bundleIndex{Format: 1, ClusterID: cid, CreatedAt: now.UTC().Format(time.RFC3339), OriginPrefix: prefix, Logs: []bundleLog{}, Anchors: []string{}, Notes: req.Notes}
	for _, l := range chosen {
		m := bundleLogPathRE.FindStringSubmatch(l.Origin)
		if m == nil {
			return info, fmt.Errorf("ledgerpub: the origin %q has no log path", l.Origin)
		}
		lp := m[1]
		cp, err := s.Object(ctx, l.Origin, "checkpoint")
		if err != nil {
			return info, fmt.Errorf("the checkpoint of %s: %w", lp, err)
		}
		if err := put(lp+"/checkpoint", cp.Data); err != nil {
			return info, err
		}
		keys, err := ledgerlog.Required(uint64(l.Size))
		if err != nil {
			return info, err
		}
		for _, k := range keys {
			if k.Level == ledgerlog.EntryLevel && req.SinceSize > 0 && int64((k.Index+1)*ledgerlog.TileWidth) <= req.SinceSize && k.Full() {
				continue
			}
			o, err := s.Object(ctx, l.Origin, k.Path())
			if err != nil {
				return info, fmt.Errorf("%s/%s: %w", lp, k.Path(), err)
			}
			if err := put(lp+"/"+k.Path(), o.Data); err != nil {
				return info, err
			}
		}
		bl := bundleLog{Kind: string(l.Kind), Origin: l.Origin, Checkpoint: lp + "/checkpoint", Size: l.Size}
		if l.Kind == KindChain {
			bl.ChainID = fmt.Sprintf("%x", l.ChainID)
			bl.Epoch = l.Epoch
		}
		idx.Logs = append(idx.Logs, bl)
		info.Logs++
		if l.Kind == KindAggregate && req.IncludeAnchors {
			cps, err := s.anchorEvidencePaths(ctx, l)
			if err != nil {
				return info, err
			}
			for _, rest := range cps {
				o, err := s.Object(ctx, l.Origin, rest)
				if err != nil {
					if errors.Is(err, ErrNotFound) {
						continue
					}
					return info, err
				}
				if err := put(lp+"/"+rest, o.Data); err != nil {
					return info, err
				}
				idx.Anchors = append(idx.Anchors, lp+"/"+rest)
			}
		}
	}
	if err := root("logs.json"); err != nil && !errors.Is(err, ErrNotFound) {
		return info, err
	}
	for name, data := range req.Files {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			return info, errors.New("ledgerpub: a file of the request has a path outside the bundle")
		}
		if err := put(name, data); err != nil {
			return info, err
		}
		idx.Proofs = append(idx.Proofs, bundleProof{File: name})
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		return info, err
	}
	if err := put("bundle.json", append(raw, '\n')); err != nil {
		return info, err
	}
	if err := tw.Close(); err != nil {
		return info, err
	}
	return info, gz.Close()
}

// anchorEvidencePaths lists the evidence files (anchors/<size>/<target>.json) of the anchored records of an aggregate log.
func (s PGStore) anchorEvidencePaths(ctx context.Context, row LogRow) ([]string, error) {
	var out []string
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT size, target_id FROM kingres_cp.ledger_anchor
			WHERE log_id = $1 AND state IN ('anchored', 'submitted', 'claimed') AND evidence IS NOT NULL ORDER BY size, target_id`, row.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var size, target int64
			if err := rows.Scan(&size, &target); err != nil {
				return err
			}
			out = append(out, fmt.Sprintf("anchors/%d/%d.json", size, target))
		}
		return rows.Err()
	})
	return out, err
}

// AuditPublication re-reads every tile and entry bundle that a database's logs hold and compares them with the root of the latest stored
// checkpoint (ledgerlog.Audit: the audit that the verifier's consistency command does not make), and checks that the note of that
// checkpoint says the size and the root that are stored. It returns the findings, one sentence each; none means the stored publication is
// intact. A diverged log is audited up to the checkpoint it has (what it holds must still be intact); a log with no checkpoint is skipped.
// The signatures are not checked here: the keys that sign are the control plane's own, and the verifier is the check that does not trust them.
func (s PGStore) AuditPublication(ctx context.Context, db string) ([]string, error) {
	logs, err := s.Logs(ctx)
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, l := range logs {
		if l.Database != db || l.Size == 0 {
			continue
		}
		cp, err := s.LatestCheckpoint(ctx, l.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		var root ledgerlog.Hash
		copy(root[:], cp.Root)
		if err := ledgerlog.Audit(ctx, pgTiles{s: s, id: l.ID}, uint64(cp.Size), root); err != nil { //nolint:gosec // a stored size
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			findings = append(findings, fmt.Sprintf("the stored tiles of %s do not give the root of the checkpoint of size %d: %v", l.Origin, cp.Size, err))
			continue
		}
		n, err := ledgerlog.ParseNote(cp.Note)
		if err != nil {
			findings = append(findings, fmt.Sprintf("the stored note of %s at size %d cannot be read: %v", l.Origin, cp.Size, err))
			continue
		}
		c, err := ledgerlog.ParseCheckpoint(n.Text)
		if err != nil || c.Origin != l.Origin || int64(c.Size) != cp.Size || !bytesEqual(c.Root[:], cp.Root) { //nolint:gosec // a parsed size
			findings = append(findings, fmt.Sprintf("the stored note of %s does not say the size and the root that are stored for size %d", l.Origin, cp.Size))
		}
	}
	return findings, nil
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

type bundleProof struct {
	File string `json:"file"`
}
