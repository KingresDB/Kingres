// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLSTATEs of the extension that the loop of promote_node tells apart (README of
// kingres_shard, "SQLSTATEs of the answers"). The loop decides by the code, never by
// the text; a broken connection or a client time-out has no code.
const (
	StateNoAnswer        = "KS007"
	StateCatalogBusy     = "KS008"
	StateCannotCommit    = "KS009"
	StateWrongNode       = "KS010"
	StateInRecovery      = "KS011"
	StateNotSafe         = "KS012"
	StateInDoubt         = "KS013"
	StateUnknownSecret   = "KS014"
	StateNoConfirmation  = "KS005"
	StateLockNotAvail    = "55P03"
	StateNoSuchNode      = "42704"
	stateConnectionClass = "08"
)

// PromoteOptions are the bounds and the hooks of the loop. Zero values are the
// defaults of the design (3.3 F7).
type PromoteOptions struct {
	// MaxCalls is the count of calls that do not end at "already" (default 10).
	MaxCalls int
	// Pause is the wait between two calls (default 2 s).
	Pause time.Duration
	// Budget is the time of the whole loop (default 15 minutes): the README bounds
	// the count of calls and not their duration, and one call can last minutes.
	Budget time.Duration
	// WaitBound is how long a node that is still in recovery or does not accept
	// connections is waited for (default 2 minutes).
	WaitBound time.Duration
	// NewNode is the node to promote and Group its group (for the texts).
	NewNode int32
	Group   int32
	// DB is the database whose catalog is moved.
	DB string
	// Sleep waits; the default honours ctx.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Probe says whether every node can answer a statement now: it is called after a
	// KS007, which is never repeated against a node that did not answer before it
	// has been asked (the node may be healthy and slow, or stuck).
	Probe func(ctx context.Context) bool
	// ReapplyList re-applies the empty list on NEW (a KS009 for NEW's own list).
	ReapplyList func(ctx context.Context) error
	// Detail receives a line for the step detail.
	Detail func(string)
	// Event records an answer (design 3.3 F7: every answer is recorded).
	Event func(kind string, detail map[string]any)
}

func (o PromoteOptions) defaults() PromoteOptions {
	if o.MaxCalls <= 0 {
		o.MaxCalls = 10
	}
	if o.Pause <= 0 {
		o.Pause = 2 * time.Second
	}
	if o.Budget <= 0 {
		o.Budget = 15 * time.Minute
	}
	if o.WaitBound <= 0 {
		o.WaitBound = 2 * time.Minute
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Detail == nil {
		o.Detail = func(string) {}
	}
	if o.Event == nil {
		o.Event = func(string, map[string]any) {}
	}
	return o
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// PromoteOutcome is what the loop reached.
type PromoteOutcome struct {
	Term      int64
	Unreached []int32
	Calls     int
}

// RunPromoteNode repeats kingres.promote_node(NewNode) by the rules of the README's
// sequences 2 and 9 until it answers "already" with final true, and says what the
// caller must do when it does not: a *PauseError (an operator decides: a refusal that
// is not a retry, a foreign prepared transaction, a node that did not answer and is
// not answering now, the bounds), or a *Error (a bug of the task). Every answer is
// handed to o.Event. The loop is idempotent by the extension's design: a repeat after
// a crash of kingresd, a change of leader or a lost answer is the next call.
func RunPromoteNode(ctx context.Context, cat Catalog, o PromoteOptions) (PromoteOutcome, error) {
	o = o.defaults()
	start := o.Now()
	var out PromoteOutcome
	var lastForeign string
	var waitSince time.Time
	recovered := false
	probed := 0
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if o.Now().Sub(start) > o.Budget {
			return out, &PauseError{Reason: fmt.Sprintf("kingres.promote_node did not end within %s; read the detail of the step and the log of the map group primary, then repeat the request", o.Budget)}
		}
		if out.Calls >= o.MaxCalls {
			return out, &PauseError{Reason: fmt.Sprintf("kingres.promote_node did not answer \"already\" in %d calls; read the answers in the report, then repeat the request", o.MaxCalls)}
		}
		res, err := cat.PromoteNode(ctx, o.DB, o.NewNode)
		if err != nil {
			act, perr := classifyPromoteError(err, o)
			o.Event("promoteNodeError", map[string]any{"error": err.Error(), "action": act.name})
			switch act.kind {
			case actFail:
				return out, perr
			case actAsk:
				return out, perr
			case actWait:
				if waitSince.IsZero() {
					waitSince = o.Now()
				}
				if o.Now().Sub(waitSince) > o.WaitBound {
					return out, fail(CodePromoteIncomplete, true, "the node to promote did not become ready within %s: %s", o.WaitBound, err.Error())
				}
				o.Detail("waiting: " + act.name)
			case actList:
				o.Detail("repeating after re-applying the empty list on the new primary")
				if o.ReapplyList != nil {
					if rerr := o.ReapplyList(ctx); rerr != nil && !errors.Is(rerr, ErrUnreachable) {
						return out, rerr
					}
				}
				out.Calls++
			case actProbe:
				probed++
				if probed > 2 || o.Probe == nil || !o.Probe(ctx) {
					return out, &PauseError{Reason: "a node accepted the session of kingres.promote_node and did not answer (" + err.Error() +
						"); it is not repeated against that node until it answers: look at the node, then repeat the request"}
				}
				out.Calls++
			case actRepeat:
				out.Calls++
			}
			if err := o.Sleep(ctx, o.Pause); err != nil {
				return out, err
			}
			continue
		}
		waitSince = time.Time{}
		out.Calls++
		out.Term = res.Term
		out.Unreached = res.Unreached
		o.Event("promoteNode", map[string]any{"status": res.Status, "term": res.Term, "final": res.Final, "unreached": res.Unreached, "blockedBy": res.Blocked})
		if len(res.Blocked) > 0 {
			var foreign []string
			for _, b := range res.Blocked {
				if b.Class != "cluster" {
					foreign = append(foreign, fmt.Sprintf("%s on node %d", b.Gid, b.Node))
				}
			}
			if len(foreign) > 0 {
				sig := strings.Join(foreign, ",")
				if sig == lastForeign {
					return out, &PauseError{Reason: "the prepared transaction(s) " + sig + " are not the cluster's and hold a lock that the catalog change needs; end them with ROLLBACK PREPARED on that node if they are unwanted, then repeat the request"}
				}
				lastForeign = sig
			} else {
				lastForeign = ""
			}
			if !recovered || foreignFree(res.Blocked) {
				if rerr := cat.RecoverPrepared(ctx); rerr != nil {
					o.Event("recoverPreparedError", map[string]any{"error": rerr.Error()})
				}
				recovered = true
			}
			o.Detail("a prepared transaction is in the way; recovery was asked")
		} else {
			lastForeign = ""
		}
		if res.Status == "already" && res.Final && len(res.Blocked) == 0 {
			return out, nil
		}
		o.Detail(fmt.Sprintf("promote_node answered %s (final %v); repeating", res.Status, res.Final))
		if err := o.Sleep(ctx, o.Pause); err != nil {
			return out, err
		}
	}
}

func foreignFree(b []Blocked) bool {
	for _, x := range b {
		if x.Class != "cluster" {
			return false
		}
	}
	return true
}

type actionKind int

const (
	actRepeat actionKind = iota
	actWait
	actList
	actProbe
	actAsk
	actFail
)

type action struct {
	kind actionKind
	name string
}

// classifyPromoteError maps the error of one call to what the loop does (table of
// design 3.3 F7). The second result is the error to return for actAsk and actFail.
func classifyPromoteError(err error, o PromoteOptions) (action, error) {
	var se *SQLError
	if !errors.As(err, &se) {
		// A broken connection, a client time-out: the call did not finish (sequence 9).
		return action{actRepeat, "the call did not finish"}, nil
	}
	switch {
	case se.Code == StateUnknownSecret:
		return action{actAsk, "KS014"}, &PauseError{Reason: "a node holds a cluster secret that no node has (" + se.Text +
			"): this is not a retry; rebuild that standby from a node that has the current secret, or promote another standby of the group, then repeat the request"}
	case se.Code == StateNotSafe:
		return action{actAsk, "KS012"}, &PauseError{Reason: "promoting is not safe: " + se.Text + ". This is a decision of the operator; nothing was changed"}
	case se.Code == StateWrongNode:
		if strings.Contains(se.Text, "only run on the map group primary") {
			return action{actFail, "wrong node"}, fail(CodeNotMapPrimary, true, "the connection of the leader is not to the map group primary: %s", se.Text)
		}
		return action{actWait, "the map group promotion is not finished"}, nil
	case se.Code == StateNoSuchNode:
		return action{actFail, "no such node"}, fail(CodeBadPromoteTarget, false, "the catalog has no node %d: %s", o.NewNode, se.Text)
	case se.Code == StateNoAnswer:
		return action{actProbe, "KS007"}, nil
	case se.Code == StateCannotCommit:
		if strings.HasPrefix(se.Text, "this node") || mentionsNode(se.Text, o.NewNode) {
			return action{actList, "KS009 on the new primary"}, nil
		}
		return action{actWait, "the synchronous standbys of another group's primary do not confirm"}, nil
	case se.Code == StateNoConfirmation:
		return action{actWait, "a participant's standby does not confirm (KS005)"}, nil
	case se.Code == StateInRecovery:
		return action{actWait, "the node is still in recovery"}, nil
	case se.Code == StateCatalogBusy, se.Code == StateLockNotAvail:
		return action{actWait, "another catalog call or a lock on a node: " + se.Code}, nil
	case se.Code == StateInDoubt:
		return action{actWait, "distributed transactions are in doubt"}, nil
	case strings.HasPrefix(se.Code, stateConnectionClass):
		return action{actWait, "a node does not accept connections"}, nil
	case se.Code == "":
		return action{actRepeat, "the call did not finish"}, nil
	}
	// Any other answer is the extension's own refusal of the input: nothing here can
	// make it succeed, so the operator reads it.
	return action{actAsk, "other"}, &PauseError{Reason: "kingres.promote_node refused: " + se.Code + " " + se.Text}
}

func mentionsNode(text string, id int32) bool {
	return strings.Contains(text, fmt.Sprintf("node %d ", id)) || strings.Contains(text, fmt.Sprintf("node %d(", id))
}
