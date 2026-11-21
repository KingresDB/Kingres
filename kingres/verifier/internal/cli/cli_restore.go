// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package cli

import (
	"fmt"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
)

// The restore-acknowledged events of a publication (design 9.4, ADR 0025, P7R-09). When an operator accepts that the system database was
// restored to an older state than what was published, the publisher signs the event and goes on from the restored database: the aggregate log
// and the events show a fork, or a smaller size, against a checkpoint held from before. The verifier reports the event wherever it reads the
// events, and names it in the failure of a comparison with a held checkpoint of the aggregate log, so that the fork is explained and not only
// found. The event is the operator's statement signed by an authorised key; the verifier says what it states and does not judge it.

// restoreNotes are the notes for a run that read the events: one for each verified restore-acknowledged event.
func restoreNotes(w *events.Walk) []string {
	if w == nil {
		return nil
	}
	var out []string
	for _, r := range w.Restores {
		out = append(out, "finding: the publication holds a verified "+r.Text()+". The history of the aggregate log and of the events published before it was cut there: an aggregate checkpoint that you hold from before that event is not a prefix of what is published now, and the fork or the smaller size it shows is what the event says. This run did not compare anything with the history that was cut")
	}
	return out
}

// restoreRemark is what the failure of a comparison of the aggregate log with a checkpoint you hold adds when the publication holds a
// verified restore-acknowledged event: it names the event, which is the statement that explains such a failure.
func restoreRemark(w *events.Walk) string {
	if w == nil || len(w.Restores) == 0 {
		return ""
	}
	var parts []string
	for _, r := range w.Restores {
		parts = append(parts, r.Text())
	}
	return ". The publication holds the verified signed statement of the operator: " + strings.Join(parts, "; and ") +
		". If the checkpoint you hold is from before that event, this explains the failure: the aggregate log and the events were cut there and continue from the restored database. The statement says why the fork exists; it does not show that the cut was honest, and a checkpoint you hold from after the event is not explained by it"
}

// restoreRemark for the commands that opened the publication.
func (p *pubInputs) restoreRemark() string {
	w, err := p.events()
	if err != nil {
		return ""
	}
	return restoreRemark(w)
}

// restoreNotes for the commands that opened the publication.
func (p *pubInputs) restoreNotes() []string {
	w, err := p.events()
	if err != nil {
		return nil
	}
	return restoreNotes(w)
}

// restoreRemarkFor is the remark for a comparison of the log in: only the aggregate log is cut by a restore; the chain logs are derived from
// the chains and continue.
func (in *logInputs) restoreRemarkFor() string {
	if in.spec.kind != "aggregate" {
		return ""
	}
	return restoreRemark(in.walk)
}

// forkRemark is the remark for the failure at step: only a fork is what the event explains, not a damaged tile.
func (p *pubInputs) forkRemark(step string) string {
	if step != "fork" {
		return ""
	}
	return p.restoreRemark()
}

// withRestore adds the remark about a restore-acknowledged event to the cause of a conflict with a held checkpoint of the aggregate log.
func (in *logInputs) withRestore(err error) error {
	if rr := in.restoreRemarkFor(); rr != "" {
		return fmt.Errorf("%w%s", err, rr)
	}
	return err
}
