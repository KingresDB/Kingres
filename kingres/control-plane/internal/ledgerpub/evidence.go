// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerpub

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// The evidence files of anchoring (design 6.2, correction 187): a/<aggregate id>/anchors/<size>/<target id>.json, the file of internal/anchor
// (EncodeEvidenceFile), served below the aggregate log it belongs to. It exists once the record holds evidence (anchored, or submitted or
// claimed with something to keep), is public by design (a note, a digest, a version id, a token), changes while an OpenTimestamps record
// moves from claimed to anchored (so it is served with the short cache of a checkpoint), is never listed (no path of the publication lists
// anything), and is not served for a record without evidence, superseded, failed or expired.

// parseEvidencePath reads "anchors/<size>/<target id>.json" in its canonical form only.
func parseEvidencePath(rest string) (size, target int64, ok bool) {
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] != "anchors" || !strings.HasSuffix(parts[2], ".json") {
		return 0, 0, false
	}
	num := func(s string) (int64, bool) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != s {
			return 0, false
		}
		return n, true
	}
	size, ok1 := num(parts[1])
	target, ok2 := num(strings.TrimSuffix(parts[2], ".json"))
	return size, target, ok1 && ok2
}

func (s PGStore) evidenceFile(ctx context.Context, row LogRow, rest string) (Object, error) {
	size, target, ok := parseEvidencePath(rest)
	if !ok || row.Kind != KindAggregate {
		return Object{}, ErrNotFound
	}
	var kind, origin string
	var digest []byte
	var evidence string
	var at, updated time.Time
	var anchoredAt *time.Time
	var sz int64
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT kind, origin, size, payload_sha256, evidence, anchored_at, updated_at FROM kingres_cp.ledger_anchor
			WHERE log_id = $1 AND size = $2 AND target_id = $3 AND state IN ('anchored', 'submitted', 'claimed') AND evidence IS NOT NULL`,
			row.ID, size, target).Scan(&kind, &origin, &sz, &digest, &evidence, &anchoredAt, &updated)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, err
	}
	at = updated
	if anchoredAt != nil {
		at = *anchoredAt
	}
	data, err := anchor.EncodeEvidenceFile(anchor.EvidenceFile{Kind: kind, Origin: origin, Size: uint64(sz), PayloadSHA256: hex.EncodeToString(digest), CreatedAt: at, Evidence: []byte(evidence)}) //nolint:gosec // size is at least 1
	if err != nil {
		return Object{}, ErrNotFound
	}
	return Object{Data: data, Mutable: true, ContentType: "application/json"}, nil
}
