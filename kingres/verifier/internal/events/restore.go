// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package events

import (
	"fmt"
	"strconv"
)

// The event restore-acknowledged (ledger-phase7.md 9.4, ADR 0025): an operator accepted that the system database of the cluster was restored to an
// older state than what was published, and the publisher goes on from the restored database. The text:
//
//	generation: <n>                  the generation of the record of what was published, from 1 (the count of accepted restores)
//	published-events: <n>            the events the replaced record held
//	published-logs: <n>              the logs it held
//	published-state-sha256: <64 hex> the SHA-256 of that record's state
//	database-events: <n>             the events the restored database held
//	database-logs: <n>               the logs it held
//	database-state-sha256: <64 hex>  the SHA-256 of that state
//	reason: <1 to 200 printable characters>
//	acknowledged-by: <1 to 200 printable characters>
//
// It is the public statement that the aggregate log and the events were cut: a mirror or an auditor that holds checkpoints from before it
// sees a fork or a smaller size, and this event, signed by an authorised key, says why. It is a statement of the operator; the verifier
// reports it and does not judge whether the cut was honest.

// TypeRestoreAcknowledged is the type of the event that announces an accepted restore.
const TypeRestoreAcknowledged = "restore-acknowledged"

// UnreadRestore is a restore-acknowledged event, signed by an authorised key, that is not in the strict form and was not read.
type UnreadRestore struct {
	Seq    uint64
	Reason string
}

// RestoreAcknowledged is a verified restore-acknowledged event.
type RestoreAcknowledged struct {
	Seq             uint64
	Generation      uint64
	PublishedEvents uint64
	PublishedLogs   uint64
	PublishedSHA    string
	DatabaseEvents  uint64
	DatabaseLogs    uint64
	DatabaseSHA     string
	Reason          string
	AcknowledgedBy  string
	SignedBy        []string // fingerprints of the authorised keys that signed
}

// Text says what the event states, in the words of 9.4.
func (r RestoreAcknowledged) Text() string {
	return fmt.Sprintf("event %06d (restore-acknowledged, generation %d, acknowledged by %s, reason %q) states that the system database was restored: %d event(s) and %d log(s) had been published, the restored database held %d event(s) and %d log(s), and the publication continues from the restored database",
		r.Seq, r.Generation, r.AcknowledgedBy, r.Reason, r.PublishedEvents, r.PublishedLogs, r.DatabaseEvents, r.DatabaseLogs)
}

// restoreAcknowledged reads the event strictly: every line, canonical numbers, a generation from 1 that was not acknowledged before. An event
// that deviates is not read at all (nothing of it is used) and is kept in Unread, and the walk goes on: a signed event of a type that only some
// commands interpret must not end the run of every command, and must not hide the events after it (P7F-06). The commands that report restore
// events say that this one was not read.
func (w *Walk) restoreAcknowledged(ev *Event, authorised []*Key) error {
	if err := w.readRestoreAcknowledged(ev, authorised); err != nil {
		w.UnreadRestores = append(w.UnreadRestores, UnreadRestore{Seq: ev.Seq, Reason: err.Error()})
	}
	return nil
}

func (w *Walk) readRestoreAcknowledged(ev *Event, authorised []*Key) error {
	want := map[string]bool{"generation": true, "published-events": true, "published-logs": true, "published-state-sha256": true,
		"database-events": true, "database-logs": true, "database-state-sha256": true, "reason": true, "acknowledged-by": true}
	for name := range ev.Fields {
		switch name {
		case "seq", "time", "type":
		default:
			if !want[name] {
				return fmt.Errorf("the line %q is not one of a restore-acknowledged event", name)
			}
		}
	}
	for name := range want {
		if _, ok := ev.Fields[name]; !ok {
			return fmt.Errorf("a restore-acknowledged event has no %s line", name)
		}
	}
	var r RestoreAcknowledged
	var err error
	r.Seq = ev.Seq
	num := func(name string, dst *uint64) error {
		v, err := canonicalUint(ev.Fields[name], 1<<40)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*dst = v
		return nil
	}
	for _, f := range []struct {
		name string
		dst  *uint64
	}{{"generation", &r.Generation}, {"published-events", &r.PublishedEvents}, {"published-logs", &r.PublishedLogs}, {"database-events", &r.DatabaseEvents}, {"database-logs", &r.DatabaseLogs}} {
		if err = num(f.name, f.dst); err != nil {
			return err
		}
	}
	if r.Generation < 1 {
		return fmt.Errorf("generation %q is not a number from 1", ev.Fields["generation"])
	}
	if _, err = hex32(ev.Fields["published-state-sha256"]); err != nil {
		return fmt.Errorf("published-state-sha256: %w", err)
	}
	if _, err = hex32(ev.Fields["database-state-sha256"]); err != nil {
		return fmt.Errorf("database-state-sha256: %w", err)
	}
	r.PublishedSHA, r.DatabaseSHA = ev.Fields["published-state-sha256"], ev.Fields["database-state-sha256"]
	for _, f := range []string{"reason", "acknowledged-by"} {
		if v := ev.Fields[f]; len(v) < 1 || len(v) > MaxReason {
			return fmt.Errorf("%s must be 1 to %d printable characters", f, MaxReason)
		}
	}
	r.Reason, r.AcknowledgedBy = ev.Fields["reason"], ev.Fields["acknowledged-by"]
	for _, prev := range w.Restores {
		if prev.Generation == r.Generation {
			return fmt.Errorf("the generation %s is acknowledged twice", strconv.FormatUint(r.Generation, 10))
		}
	}
	for _, k := range authorised {
		r.SignedBy = append(r.SignedBy, k.Fingerprint)
	}
	w.Restores = append(w.Restores, r)
	return nil
}
