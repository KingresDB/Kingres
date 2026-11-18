// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Verdicts of the data loss assessment (contract DataLossAssessment.verdict).
const (
	VerdictNone      = "none"
	VerdictAsyncOnly = "asyncOnly"
	VerdictPossible  = "possible"
	VerdictUnknown   = "unknown"
)

// MaxCheckAge is how old the leader's last check of a group may be and still say
// that a synchronous standby streamed (design 2.6: "a check younger than 30 s").
const MaxCheckAge = 30 * time.Second

// Candidate is a ready standby of the group with what was read of it.
type Candidate struct {
	StandbyID int64
	Server    string
	// Readable is false when its position could not be read.
	Readable bool
	// Err says why it could not be read (for the report).
	Err string
	Pos Position
	// SyncState is the state at the last check of the leader ("sync", "quorum",
	// "async", "potential" or "").
	SyncState string
	Preferred bool
	RingDist  int
}

// ChooseInput is everything the choice depends on, and nothing else: the choice is
// a pure function (design 3.6; a test per tie-break).
type ChooseInput struct {
	Candidates []Candidate
	// Target is the server the request named for the group ("" for the best).
	Target string
	// Facts are the leader's knowledge of the group.
	Facts GroupFacts
	// AllowDataLoss is only used for the text of the verdict; the caller decides.
}

// Choice is the result.
type Choice struct {
	// Chosen is the standby to promote, nil when none is eligible.
	Chosen *Candidate
	// Order is every candidate, the eligible ones first in the order of preference.
	Order []Candidate
	// Ineligible maps a standby id to the reason it cannot be promoted.
	Ineligible map[int64]string
	Verdict    string
	Message    string
	// Unreadable are the servers whose standby could not be read.
	Unreadable []string
	// BestLsn is the highest flushed position read (0 when none).
	BestLsn uint64
	// ChosenLsn is the flushed position of the chosen one.
	ChosenLsn uint64
}

// Choose orders the candidates and judges the loss. Eligible: readable, in
// recovery, on the highest timeline, with the system identifier most of the readable
// ones have. Order: the highest flushed position (the larger of ReceiveLsn and
// ReplayLsn), then a synchronous standby, then the group's preferred server, then
// the nearest in the ring, then the lowest id. A requested Target is eligible only if
// listed; the verdict then compares it with the best.
func Choose(in ChooseInput) Choice {
	out := Choice{Ineligible: map[int64]string{}}
	cands := append([]Candidate(nil), in.Candidates...)
	var maxTL int64
	idCount := map[string]int{}
	for _, c := range cands {
		if !c.Readable {
			out.Unreadable = append(out.Unreadable, c.Server)
			continue
		}
		if c.Pos.Timeline > maxTL {
			maxTL = c.Pos.Timeline
		}
		idCount[c.Pos.SystemIdentifier]++
	}
	commonID, best := "", 0
	for id, n := range idCount {
		if n > best || (n == best && id < commonID) {
			commonID, best = id, n
		}
	}
	var eligible, rest []Candidate
	for _, c := range cands {
		switch {
		case !c.Readable:
			out.Ineligible[c.StandbyID] = "its position could not be read" + suffix(c.Err)
		case !c.Pos.InRecovery:
			out.Ineligible[c.StandbyID] = "it is not in recovery"
		case c.Pos.Timeline < maxTL:
			out.Ineligible[c.StandbyID] = "it is on an older timeline"
		case c.Pos.SystemIdentifier != commonID:
			out.Ineligible[c.StandbyID] = "it is not a copy of the same cluster"
		default:
			if _, ok := c.Pos.Flushed(); !ok {
				out.Ineligible[c.StandbyID] = "it reports no WAL position"
				break
			}
			eligible = append(eligible, c)
			continue
		}
		rest = append(rest, c)
	}
	sort.SliceStable(eligible, func(i, j int) bool { return before(eligible[i], eligible[j]) })
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].StandbyID < rest[j].StandbyID })
	out.Order = append(eligible, rest...)
	if len(eligible) > 0 {
		out.BestLsn, _ = eligible[0].Pos.Flushed()
	}
	if in.Target != "" {
		found := false
		for i := range eligible {
			if eligible[i].Server == in.Target {
				found = true
				out.Chosen = &out.Order[i]
				break
			}
		}
		if !found {
			out.Verdict = VerdictUnknown
			out.Message = fmt.Sprintf("the requested standby on %s is not an eligible candidate", in.Target)
			return out
		}
	} else if len(eligible) > 0 {
		out.Chosen = &out.Order[0]
	}
	if out.Chosen == nil {
		out.Verdict = VerdictUnknown
		out.Message = "no standby is eligible"
		var why []string
		for _, c := range cands {
			if r, ok := out.Ineligible[c.StandbyID]; ok {
				why = append(why, fmt.Sprintf("standby %d on %s: %s", c.StandbyID, c.Server, r))
			}
		}
		if len(why) > 0 {
			out.Message += " (" + strings.Join(why, "; ") + ")"
		}
		return out
	}
	out.ChosenLsn, _ = out.Chosen.Pos.Flushed()
	sort.Strings(out.Unreadable)
	out.Verdict, out.Message = judge(in, out)
	return out
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// before is the order of preference of two eligible candidates.
func before(a, b Candidate) bool {
	la, _ := a.Pos.Flushed()
	lb, _ := b.Pos.Flushed()
	if la != lb {
		return la > lb
	}
	sa, sb := syncRank(a.SyncState), syncRank(b.SyncState)
	if sa != sb {
		return sa < sb
	}
	if a.Preferred != b.Preferred {
		return a.Preferred
	}
	if a.RingDist != b.RingDist {
		return a.RingDist < b.RingDist
	}
	return a.StandbyID < b.StandbyID
}

func syncRank(s string) int {
	switch s {
	case "sync", "quorum":
		return 0
	case "potential":
		return 1
	}
	return 2
}

// judge is the verdict of design section 2.6. unknown: a ready standby could not be
// read (a standby that cannot be read must be assumed to hold what the others lack).
// possible: the group ran without a synchronous standby streaming at the last check,
// or the target is behind the best. asyncOnly: a standby streamed synchronously and
// the best is chosen, but some database of the group commits asynchronously, so only
// those commits can be lost. none: the same and no database is asynchronous.
func judge(in ChooseInput, c Choice) (string, string) {
	if len(c.Unreadable) > 0 {
		return VerdictUnknown, "the standby on " + strings.Join(c.Unreadable, ", ") + " could not be read; it may hold commits that the chosen one lacks"
	}
	if c.ChosenLsn < c.BestLsn {
		return VerdictPossible, fmt.Sprintf("the requested standby is %d bytes behind the best (%s against %s); the difference is lost",
			c.BestLsn-c.ChosenLsn, FormatLSN(c.ChosenLsn), FormatLSN(c.BestLsn))
	}
	f := in.Facts
	if !f.SyncStreaming || f.LastKnownAge > MaxCheckAge || f.LastKnownAge < 0 {
		return VerdictPossible, "the group did not stream a synchronous standby at its last check; commits acknowledged after the standby stopped confirming may be lost"
	}
	if len(f.AsyncDatabases) > 0 {
		return VerdictAsyncOnly, "only commits of the asynchronous databases (" + strings.Join(f.AsyncDatabases, ", ") + ") acknowledged after the standby's last confirmed position can be lost"
	}
	return VerdictNone, "the chosen standby holds every commit that was acknowledged under synchronous durability"
}

// Accepts says whether a request may proceed with this verdict: none always, any
// other only when the operator accepted the loss.
func Accepts(verdict string, allowDataLoss bool) bool {
	return verdict == VerdictNone || allowDataLoss
}
