// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// ErrNotHeldBack is returned by ReleaseHold when the lease directory holds no hand-over marker of the group.
var ErrNotHeldBack = errors.New("lease: the primary of the group is not held back for a hand-over")

// ReleaseHold is the offline end of the hold of a primary that a planned hand-over left behind, for the case that no server can end it (the operator's local command
// of kingresd, with kingresd stopped; nothing else calls it). dir is the lease directory of the data directory. It needs the hand-over marker of the group, and refuses
// when the hand-over was recorded for another server than lost: the declaration must name the server the group was handed to (or the hold has no record). It writes the
// wait marker first (a primary whose lease is enforced does not start before it holds one), then removes the hand-over marker and the record of the candidate, in that
// order, so that a crash in between is repeated by the same call. It returns the candidate that was recorded ("" for none).
func ReleaseHold(dir string, group int32, lost string) (candidate string, err error) {
	if !markerExists(dir, RelinquishedMarker, group) {
		return "", ErrNotHeldBack
	}
	if b, rerr := os.ReadFile(MarkerPath(dir, CandidateMarker, group)); rerr == nil {
		candidate = strings.TrimSpace(string(b))
	}
	if candidate != "" && candidate != lost {
		return candidate, fmt.Errorf("lease: the hold was made for %s, not for %s", candidate, lost)
	}
	if err := pki.WriteFile(MarkerPath(dir, WaitMarker, group), []byte("waiting for the group lease\n"), 0o600); err != nil {
		return candidate, fmt.Errorf("lease: writing the wait marker: %w", err)
	}
	for _, base := range []string{RelinquishedMarker, CandidateMarker} {
		if err := os.Remove(MarkerPath(dir, base, group)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return candidate, fmt.Errorf("lease: removing the %s marker: %w", base, err)
		}
	}
	if markerExists(dir, RelinquishedMarker, group) {
		// the marker of the version that had one primary per server: kingresd renames it when it starts, so it is not ours to remove here
		return candidate, errors.New("lease: a hand-over marker without a group number remains; start kingresd once and repeat the command")
	}
	return candidate, nil
}
