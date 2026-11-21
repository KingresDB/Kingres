// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// The key events of a publication (key-retired, key-revoked) change which keys may vouch for a signature
// (design 3.3, corrections 165 and 168). Every command that judges a signature of a publication goes through
// one rule: the trust file the auditor pinned is narrowed by the events that verify under it (a key that a
// verified event revoked vouches for nothing, a key that a verified event retired vouches only up to the
// size the event names), and the narrowed file is what the signed notes are verified under. A key an event
// adds is not used by these commands: trust moves only through `trust follow`, whose file the auditor pins.

// maxListedSkipped is how many unverified events a note lists before it counts the rest.
const maxListedSkipped = 20

// readEvents reads the events of a source under the keys of the file and checks the listing of the events
// directory for a gap when the source can list. The walk returned is usable up to the error.
func readEvents(base *trust.File, src pubsrc.Source, strict bool) (*events.Walk, error) {
	w, err := events.Follow(base, events.FromSource(src.ReadFile), strict)
	if err != nil {
		return w, err
	}
	if names, lerr := src.Files("events"); lerr == nil {
		if gerr := w.CheckGap(names); gerr != nil {
			return w, gerr
		}
	}
	return w, nil
}

// applyKeyEvents narrows base by the verified key events of w and says what it did. base is not changed.
func applyKeyEvents(base *trust.File, w *events.Walk) (*trust.File, []string) {
	eff := base.Clone()
	var notes []string
	for _, k := range w.Keys {
		fp := k.Fingerprint[:16]
		switch {
		case !k.Pinned:
			notes = append(notes, fmt.Sprintf("event %06d added key %s: this command does not use a key an event added (run trust follow and pin the file it writes); a checkpoint that only that key signed is not accepted here", k.AddedBy, fp))
		case k.Revoked && !k.RevokedInFile:
			cause := fmt.Sprintf("the verified event %06d (compromised since %s: %s)", k.RevokedBy, k.CompromisedSince.UTC().Format(time.RFC3339), k.RevokeReason)
			eff.Revoke(k.Fingerprint, cause)
			notes = append(notes, fmt.Sprintf("key %s is revoked by %s: no checkpoint it signed counts, whatever its size or date (an anchor that dates a checkpoint before the revocation is not read by this version)", fp, cause))
		case k.RetiredByEvent && k.MaxTreeSize != nil:
			cause := fmt.Sprintf("the verified event %06d", k.RetiredBy)
			eff.LimitTo(k.Fingerprint, *k.MaxTreeSize, cause)
		case k.RetiredByEvent:
			notes = append(notes, fmt.Sprintf("key %s was retired by the verified event %06d with no bound (\"unbounded\"): it still counts for any size", fp, k.RetiredBy))
		}
	}
	return eff, notes
}

// skippedNotes lists the events that were read and that no authorised key signed.
func skippedNotes(w *events.Walk) []string {
	var out []string
	for i, sk := range w.Skipped {
		if i == maxListedSkipped {
			out = append(out, fmt.Sprintf("and %d more event(s) that no authorised key signed", len(w.Skipped)-maxListedSkipped))
			break
		}
		out = append(out, fmt.Sprintf("event %06d (%s) was read and is NOT verified, it counts for nothing: %s", sk.Seq, sk.Type, sk.Reason))
	}
	return out
}

// eventsGateOutcome is the outcome of an error of reading the events when a command needs them to know
// which keys may vouch: a source that cannot be reached is "could not check", anything else is a failure
// of the events.
func eventsGateOutcome(err error) outcome {
	var ge *events.Error
	if errors.As(err, &ge) {
		return eventsOutcome(err, "the events of the publication could not be read, and the keys that may vouch depend on them")
	}
	return eventsOutcome(err, "the events of the publication could not be read")
}

// gate is nil when the events were read, and otherwise the outcome that ends a command that cannot go on
// without them (they decide which keys count).
func (p *pubInputs) gate() outcome {
	if p.walkErr == nil {
		return outcome{}
	}
	return eventsGateOutcome(p.walkErr)
}

// applyEvents narrows the trust file of a single-log command by the verified key events of its source. A
// problem with the events is a problem of the publication (status 3 when they cannot be reached, 1 when
// they are wrong or have a gap), because the keys that may vouch depend on them.
func (in *logInputs) applyEvents() error {
	d, err := in.openSource()
	if err != nil {
		return err
	}
	w, werr := readEvents(in.trust, d, false)
	if werr != nil {
		switch {
		case errors.Is(werr, pubsrc.ErrRedirect), errors.Is(werr, pubsrc.ErrNoListing):
			return werr
		case errors.Is(werr, pubsrc.ErrUnreachable):
			return &pubError{pubUnreachable, "the events (they decide which keys may vouch)", werr}
		}
		return &pubError{pubMalformed, "the events", werr}
	}
	in.walk = w
	var kn []string
	in.trust, kn = applyKeyEvents(in.trust, w)
	in.notes = append(in.notes, kn...)
	in.notes = append(in.notes, skippedNotes(w)...)
	in.notes = append(in.notes, in.closedEpochNote(w)...)
	return nil
}
