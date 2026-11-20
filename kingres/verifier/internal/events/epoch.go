// SPDX-License-Identifier: AGPL-3.0-only

package events

import (
	"fmt"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
)

// The event epoch-closed (ledger-phase7.md 9.4): the operator acknowledged that the chain of a database was
// restored to an earlier state or lost its tail, and the publication began a new epoch of that chain's log. The text:
//
//	chain: <64 hex>            the chain
//	epoch: <n>                 the epoch that was closed (from 1)
//	final-size: <n>            the size of the closed log, the last one published
//	final-root: <64 hex>       its root at that size
//	common-height: <n | -1 | unknown>   the highest height whose block is the same in both histories
//	kind: <restore | failover-loss | other>
//	reason: <1 to 200 printable characters>
//	acknowledged-by: <1 to 200 printable characters>
//
// "unknown" is what the publisher writes today (the old blocks are no longer in the database); -1 means no block is
// common. The event is a statement of the operator signed by the server's key: it says what the operator claims.

// TypeEpochClosed is the type of the event that closes an epoch.
const TypeEpochClosed = "epoch-closed"

// EpochClosed is a verified epoch-closed event.
type EpochClosed struct {
	Seq       uint64
	Chain     [32]byte
	Epoch     uint32
	FinalSize uint64
	FinalRoot [32]byte
	// CommonKnown is false for "unknown"; CommonHeight is -1 when no block is common.
	CommonKnown    bool
	CommonHeight   int64
	Kind           string
	Reason         string
	AcknowledgedBy string
	SignedBy       []string // fingerprints of the authorised keys that signed
}

// CommonText is the common height as a person reads it.
func (c EpochClosed) CommonText() string {
	switch {
	case !c.CommonKnown:
		return "common height not stated"
	case c.CommonHeight < 0:
		return "no common block"
	}
	return "common height " + strconv.FormatInt(c.CommonHeight, 10)
}

func (w *Walk) epochClosed(ev *Event, authorised []*Key) error {
	want := map[string]bool{"chain": true, "epoch": true, "final-size": true, "final-root": true, "common-height": true, "kind": true, "reason": true, "acknowledged-by": true}
	for name := range ev.Fields {
		switch name {
		case "seq", "time", "type":
		default:
			if !want[name] {
				return fmt.Errorf("the line %q is not one of an epoch-closed event", name)
			}
		}
	}
	for name := range want {
		if _, ok := ev.Fields[name]; !ok {
			return fmt.Errorf("an epoch-closed event has no %s line", name)
		}
	}
	var ec EpochClosed
	var err error
	ec.Seq = ev.Seq
	if ec.Chain, err = hex32(ev.Fields["chain"]); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	e, err := canonicalUint(ev.Fields["epoch"], 1<<31)
	if err != nil || e < 1 {
		return fmt.Errorf("epoch %q is not a number from 1", ev.Fields["epoch"])
	}
	ec.Epoch = uint32(e) // #nosec G115 -- range checked
	if ec.FinalSize, err = canonicalUint(ev.Fields["final-size"], tlog.MaxTreeSize); err != nil || ec.FinalSize < 1 {
		return fmt.Errorf("final-size %q is not a size from 1", ev.Fields["final-size"])
	}
	if ec.FinalRoot, err = hex32(ev.Fields["final-root"]); err != nil {
		return fmt.Errorf("final-root: %w", err)
	}
	switch c := ev.Fields["common-height"]; c {
	case "unknown":
	case "-1":
		ec.CommonKnown, ec.CommonHeight = true, -1
	default:
		h, err := canonicalUint(c, ec.FinalSize)
		if err != nil {
			return fmt.Errorf("common-height %q is neither unknown, -1 nor a height below the final size %d", c, ec.FinalSize)
		}
		ec.CommonKnown, ec.CommonHeight = true, int64(h) // #nosec G115 -- below the final size
	}
	switch k := ev.Fields["kind"]; k {
	case "restore", "failover-loss", "other":
		ec.Kind = k
	default:
		return fmt.Errorf("kind %q is not one of restore, failover-loss, other", k)
	}
	for _, f := range []string{"reason", "acknowledged-by"} {
		if v := ev.Fields[f]; len(v) < 1 || len(v) > MaxReason {
			return fmt.Errorf("%s must be 1 to %d printable characters", f, MaxReason)
		}
	}
	ec.Reason, ec.AcknowledgedBy = ev.Fields["reason"], ev.Fields["acknowledged-by"]
	key := ChainKey(ec.Chain, ec.Epoch)
	if _, dup := w.EpochClosed[key]; dup {
		return fmt.Errorf("the epoch %s is closed twice", key)
	}
	for _, k := range authorised {
		ec.SignedBy = append(ec.SignedBy, k.Fingerprint)
	}
	w.EpochClosed[key] = ec
	return nil
}
