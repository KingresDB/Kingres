// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"sort"
	"strings"
	"time"
)

// SyncInput is what the rule looks at in one evaluation. Times are monotonic
// durations (package clock), never wall-clock times.
type SyncInput struct {
	// Now is the monotonic time of this evaluation.
	Now time.Duration
	// Planned are the application names of the group's standbys that exist
	// (created and started); Streaming says which of them are connected and
	// streaming to the primary.
	Planned   []string
	Streaming map[string]bool
	// Applied is the synchronous_standby_names the primary has now.
	Applied string
	// Frozen is true while a failover of the group is being decided: the setting is
	// never cleared then (removing the last streaming standby from the list while a
	// failover is decided could make the old primary acknowledge commits the new
	// one will not have).
	Frozen bool
	// Grace is how long no standby may stream before the group stops waiting.
	Grace time.Duration
}

// SyncDecision is the outcome of one evaluation.
type SyncDecision struct {
	// Set is the new synchronous_standby_names, nil when it stays as it is.
	Set *string
	// WithoutSynchronousStandby is true while the group has standbys that do not
	// stream for longer than the grace period: commits are acknowledged by the
	// primary alone (the alert groupWithoutSynchronousStandby).
	WithoutSynchronousStandby bool
}

// SyncState remembers, between evaluations, since when no standby has streamed.
type SyncState struct {
	noneSince *time.Duration
}

// Names renders the setting for the planned standbys: ANY 1 (a, b), or "" for none.
func Names(planned []string) string {
	if len(planned) == 0 {
		return ""
	}
	p := append([]string(nil), planned...)
	sort.Strings(p)
	return "ANY 1 (" + strings.Join(p, ", ") + ")"
}

// Decide applies the rule of the design:
//
//   - synchronous_standby_names is ANY 1 (<all the group's standbys>) when at
//     least one of them is connected and streaming;
//   - when none streams, the setting is left alone for the grace period (a standby
//     that reconnects within it never made the group wait), and emptied only after
//     it, so that the group keeps accepting writes without its standby; that is
//     reported (WithoutSynchronousStandby) until a standby streams again;
//   - it is never changed at all while a failover of the group is decided (Frozen);
//   - a group that has no standby at all has an empty setting, at once.
//
// A setting that is empty because the group has not had a streaming standby yet
// is not set before one streams: a new standby never makes the group wait for it
// while its base backup catches up.
func (s *SyncState) Decide(in SyncInput) SyncDecision {
	d := s.decide(in)
	// Nothing changes the list of a frozen group, in any branch (invariant I2 of the design, F2-01): not emptied, not
	// replaced, not widened, and no name is added to an empty one. A failover reads the positions of the standbys the
	// list names and picks the highest; a list that changed meanwhile could be acknowledged by a standby it did not read.
	// The time without a streaming standby is still counted, so that the grace has passed when the freeze lapses.
	if in.Frozen {
		d.Set = nil
	}
	return d
}

func (s *SyncState) decide(in SyncInput) SyncDecision {
	var d SyncDecision
	want := Names(in.Planned)
	if len(in.Planned) == 0 {
		s.noneSince = nil
		// A frozen group's list is never changed (invariant I2 of the design): the standby that is not planned
		// now (a rebuild, a lost slot, a plan that dropped it) may be the one a failover is deciding on.
		if in.Applied != "" && !in.Frozen {
			e := ""
			d.Set = &e
		}
		return d
	}
	streaming := false
	for _, p := range in.Planned {
		if in.Streaming[p] {
			streaming = true
		}
	}
	if streaming {
		s.noneSince = nil
		if in.Applied != want {
			d.Set = &want
		}
		return d
	}
	if s.noneSince == nil {
		now := in.Now
		s.noneSince = &now
	}
	waited := in.Now - *s.noneSince
	if in.Applied != "" {
		if waited > in.Grace && !in.Frozen {
			e := ""
			d.Set = &e
			d.WithoutSynchronousStandby = true
		}
		return d
	}
	d.WithoutSynchronousStandby = waited > in.Grace
	return d
}

// ListedNames returns the names in a synchronous_standby_names value as the
// primary has it ("ANY 1 (a, b)", "FIRST 2 (a, b)", "a, b", names in double
// quotes). Names compare without regard to case in PostgreSQL, so the result is
// keyed by the lower-cased name.
func ListedNames(list string) map[string]bool {
	out := map[string]bool{}
	s := strings.TrimSpace(list)
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[i+1:]
		if j := strings.LastIndexByte(s, ')'); j >= 0 {
			s = s[:j]
		}
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p != "" {
			out[strings.ToLower(p)] = true
		}
	}
	return out
}

// SafeName reports whether a standby name may be written into the list as it is:
// the extension's check (kingres.standby_name_matches) is PostgreSQL's own, but
// the list is rendered here, and a name with a quote, a comma, a space or the
// wildcard would change what the list means (kingres_shard README, sequence 1).
func SafeName(name string) bool {
	if name == "" || name == "*" {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// StreamingUnder reports whether a walsender with this application name streams,
// comparing without regard to case as the primary does.
func StreamingUnder(streaming map[string]bool, name string) bool {
	for app, ok := range streaming {
		if ok && strings.EqualFold(app, name) {
			return true
		}
	}
	return false
}
