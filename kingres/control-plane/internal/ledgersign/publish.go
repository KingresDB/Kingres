// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// What the collector (internal/ledgerround) takes from the signing keys: the settings of the publication, the keys that sign now with the
// revision of the key set they were read at, and the events of the chains (chain-retired).

// Types of the events the collector publishes (design 3.2.4 and 5.5).
const (
	EventChainRetired = "chain-retired"
)

// EventField is a "name: value" line of an event.
type EventField struct{ Name, Value string }

// PubContext is the state of the publication at one moment, read in one pass.
type PubContext struct {
	// Term is the term of this server's scheduler (0 when it holds none, then no write may be made).
	Term scheduler.Term
	// Enabled, Interval and PublicURL are the settings of the publication; Prefix is the origin prefix in force (the stored one, or the
	// default of the daemon); ClusterID is nil until the first key is made.
	Enabled   bool
	Interval  time.Duration
	PublicURL string
	Prefix    string
	ClusterID []byte
	// Locked is true once a checkpoint was signed: the origin prefix cannot change any more, and PutCheckpoint need not mark it.
	Locked bool
	// Revision is the revision of the key set that Signers were read at. A checkpoint is stored only while the revision is still this
	// one (ledgerpub.PutCheckpoint), which makes the signing of a checkpoint and the retirement of a key exclude each other.
	Revision int64
	// Signers are the keys that sign a checkpoint now, opened: every key that is active and in force, except a key whose retirement was
	// begun (its key-retired event is published; the bound in the event was decided from the checkpoints stored then, so it must not
	// sign any more). Empty when SignErr is not nil.
	Signers []ledgerkeys.Signer
	// Set is the key set as loaded.
	Set ledgerkeys.Set
	// SignErr says why no checkpoint can be signed now: ErrNoKeys, ErrNotLeader, ErrAlarm, ErrDoNotSign or ErrClockSkew (wrapped).
	SignErr error
}

// Publication returns the settings of the publication and the keys that sign now. A follower, a damaged key set, a missing key file, a
// clock that cannot be trusted and a key not yet in force are not errors of the call: they are SignErr, so that the caller still knows
// whether publication is on. The error of the call is a failure to read the state.
func (s *Service) Publication(ctx context.Context) (PubContext, error) {
	var pc PubContext
	term, leader := s.cfg.Term()
	if leader {
		pc.Term = term
	}
	v, err := s.load(ctx)
	if err != nil {
		return pc, err
	}
	c := v.snap.Cluster
	pc.Enabled = c.PublicationEnabled
	pc.Interval = time.Duration(c.PublicationInterval) * time.Second
	if c.PublicURL != nil {
		pc.PublicURL = *c.PublicURL
	}
	pc.Prefix = v.prefix
	pc.ClusterID = append([]byte(nil), c.ClusterID...)
	pc.Locked = c.FirstCheckpointAt != nil
	pc.Revision = v.snap.Revision
	pc.Set = v.set
	switch {
	case !leader:
		pc.SignErr = ErrNotLeader
		return pc, nil
	case len(v.set) == 0:
		pc.SignErr = ErrNoKeys
		return pc, nil
	case v.alarm != nil:
		pc.SignErr = v.alarm
		return pc, nil
	}
	now, err := s.clock(ctx)
	if err != nil {
		pc.SignErr = err
		return pc, nil
	}
	keys, err := v.set.Signers(now)
	switch {
	case errors.Is(err, ledgerkeys.ErrNotInForce):
		pc.SignErr = fmt.Errorf("%w: %v", ErrDoNotSign, err)
		return pc, nil
	case errors.Is(err, ledgerkeys.ErrNoActiveKey):
		pc.SignErr = fmt.Errorf("%w: %v", ErrAlarm, err)
		return pc, nil
	case err != nil:
		return pc, err
	}
	var usable []ledgerkeys.Key
	for _, k := range keys {
		if _, retiring := s.eventExists(v, EventKeyRetired, k.Fingerprint()); retiring {
			continue
		}
		usable = append(usable, k)
	}
	if len(usable) == 0 {
		pc.SignErr = fmt.Errorf("%w: every key that is in force is being retired", ErrDoNotSign)
		return pc, nil
	}
	for _, k := range usable {
		sg, err := s.cfg.Keyring.Signer(k.Fingerprint())
		if err != nil {
			pc.SignErr = fmt.Errorf("%w: the key %s cannot be opened: %v", ErrAlarm, k.Fingerprint()[:16], err)
			pc.Signers = nil
			return pc, nil
		}
		if !equalKey(sg.Public(), k.Public) {
			pc.SignErr = fmt.Errorf("%w: the key file of %s holds another key", ErrAlarm, k.Fingerprint()[:16])
			pc.Signers = nil
			return pc, nil
		}
		pc.Signers = append(pc.Signers, sg)
	}
	return pc, nil
}

// AppendEvent publishes an event of the given type about subject (a text that names the thing, unique per type: the same event is not
// published twice, a repeated step finds it), signed by every key that is active and in force. It returns the sequence number the
// event has, a new one or the one found. The lines are written in the order given; every value must be printable ASCII.
func (s *Service) AppendEvent(ctx context.Context, typ, subject string, fields []EventField) (int64, error) {
	if subject == "" {
		return 0, errors.New("ledgersign: an event of the collector names its subject")
	}
	var seq int64
	err := s.mutate(ctx, func(v *view, now time.Time) (*Change, error) {
		if e, ok := s.eventExists(v, typ, subject); ok {
			seq = e.Seq
			return nil, nil
		}
		signers := activeSigners(v.set, now)
		if len(signers) == 0 {
			return nil, &coded{code: "no_signer", msg: "no key is active and in force to sign the event", retryable: true}
		}
		fs := make([]field, len(fields))
		for i, f := range fields {
			fs[i] = field{f.Name, f.Value}
		}
		ev, err := appendEvent(v, typ, subject, recordTime(v.set, now), fs, func(text []byte) ([]byte, error) {
			return s.signEvent(ctx, v, text, ledgerkeys.PurposeCheckpoint, "", signers, now)
		})
		if err != nil {
			return nil, err
		}
		seq = ev.Seq
		return &Change{Events: []EventRow{ev}}, nil
	})
	return seq, err
}

// Prefix returns the origin prefix in force (the stored one, or the default of the daemon): what the publication listener maps a path
// below the prefix to a log origin with. It reads the state and opens no key.
func (s *Service) Prefix(ctx context.Context) (string, error) {
	snap, err := s.cfg.Store.Load(ctx)
	if err != nil {
		return "", err
	}
	return s.prefixOf(snap.Cluster), nil
}
