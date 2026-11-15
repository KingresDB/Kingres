// SPDX-License-Identifier: PostgreSQL

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
//   - it is never emptied while a failover of the group is decided (Frozen);
//   - a group that has no standby at all has an empty setting, at once.
//
// A setting that is empty because the group has not had a streaming standby yet
// is not set before one streams: a new standby never makes the group wait for it
// while its base backup catches up.
func (s *SyncState) Decide(in SyncInput) SyncDecision {
	var d SyncDecision
	want := Names(in.Planned)
	if len(in.Planned) == 0 {
		s.noneSince = nil
		if in.Applied != "" {
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
