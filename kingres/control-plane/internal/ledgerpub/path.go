// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerpub

import (
	"errors"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
)

// ErrBadPath is returned for a path that names no file of a publication.
var ErrBadPath = errors.New("ledgerpub: not a path of a publication")

// Rest is a file below a log prefix: the checkpoint, or a tile or an entry bundle.
type Rest struct {
	Checkpoint bool
	Tile       ledgerlog.TileKey
}

// ParseRest parses the path below the prefix of a log ("checkpoint", "tile/<L>/<N>[.p/<W>]", "tile/entries/<N>[.p/<W>]").
// Only the canonical form is accepted.
func ParseRest(p string) (Rest, error) {
	if p == "checkpoint" {
		return Rest{Checkpoint: true}, nil
	}
	k, err := ledgerlog.ParseTilePath(p)
	if err != nil {
		return Rest{}, ErrBadPath
	}
	return Rest{Tile: k}, nil
}

// LogPath is what a request path names: the part that identifies a log ("c/<chain>/e<epoch>" or "a/<aggregate>") and the
// path below it, or a file of the cluster ("keyring.json", "events/<seq>") with an empty log part.
type LogPath struct {
	Log  string // "c/<64 hex>/e<n>", "a/<64 hex>" or ""
	Rest string
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ParsePath splits a request path (without the leading slash) of a publication. It accepts nothing but the layout of design
// 4.6: no empty element, no dot element, no percent sign or backslash, canonical identifiers (lower-case hexadecimal, an epoch
// without leading zeros, an event number of six digits).
func ParsePath(p string) (LogPath, error) {
	if p == "" || strings.ContainsAny(p, "\\%\x00") || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return LogPath{}, ErrBadPath
	}
	parts := strings.Split(p, "/")
	for _, e := range parts {
		if e == "" || e == "." || e == ".." {
			return LogPath{}, ErrBadPath
		}
	}
	switch parts[0] {
	case "logs.json":
		if len(parts) == 1 {
			return LogPath{Rest: "logs.json"}, nil
		}
	case "keyring.json":
		if len(parts) == 1 {
			return LogPath{Rest: "keyring.json"}, nil
		}
	case "events":
		if len(parts) == 2 && len(parts[1]) == 6 {
			if _, err := strconv.ParseUint(parts[1], 10, 32); err == nil && !strings.ContainsAny(parts[1], "+-") {
				return LogPath{Rest: p}, nil
			}
		}
	case "c":
		if len(parts) >= 4 && isHex64(parts[1]) && len(parts[2]) >= 2 && parts[2][0] == 'e' {
			n, err := strconv.ParseUint(parts[2][1:], 10, 31)
			if err == nil && n >= 1 && parts[2][1] != '0' && !strings.ContainsAny(parts[2][1:], "+-") {
				return LogPath{Log: strings.Join(parts[:3], "/"), Rest: strings.Join(parts[3:], "/")}, nil
			}
		}
	case "a":
		if len(parts) >= 3 && isHex64(parts[1]) {
			return LogPath{Log: strings.Join(parts[:2], "/"), Rest: strings.Join(parts[2:], "/")}, nil
		}
	}
	return LogPath{}, ErrBadPath
}
