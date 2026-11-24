// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The old leader of a planned switchover of the map group is held back from starting from the clean stop on: its primary is the system database, so while it is
// held back no task can be read or written, and nothing that runs from the task engine can end the hold when the switchover is given up. These calls and the
// loop below end it (F6-01): the candidate asks the old leader to resolve its hold when it gives its bootstrap up, an operator can ask every server to do so, and
// the old leader asks the servers that host a standby, by itself, from the time its kingresd starts.
const (
	callMapBootstrap = "GET /failover/map/bootstrap"
	callMapResume    = "POST /failover/map/resume"
	callMapHeld      = "GET /failover/map/held"
)

// mapBootstrapStatus is what a server answers about the bootstrap of the map group that names the asking server as the old leader.
type mapBootstrapStatus struct {
	// Active says that a bootstrap of this server names the old server and has neither finished nor been given up.
	Active bool `json:"active"`
	// Standby is the state of the standby of the map group that this server hosts: "in_recovery" (it is a standby, shown by the instance or by its control data),
	// "primary" (it was promoted), "unknown" (it cannot be read) or "none" (this server hosts none).
	Standby string `json:"standby"`
}

// mapResumeAnswer is the answer of a server that was asked to resolve the hold of its primary of the map group.
type mapResumeAnswer struct {
	Resumed bool   `json:"resumed"`
	Detail  string `json:"detail,omitempty"`
}

// recoveryFromControlData reads the state of a standby that does not run from the "Database cluster state" of pg_controldata: a standby that was stopped
// says "in archive recovery" (or "shut down in recovery" after a clean stop); a primary that was stopped says "shut down" and a running one "in production".
// Any other state (a crash, a shutdown in progress) is not an answer.
func recoveryFromControlData(state string) (bool, error) {
	switch state {
	case "in archive recovery", "shut down in recovery":
		return true, nil
	case "in production", "shut down":
		return false, nil
	}
	return false, fmt.Errorf("the control data of the instance say %q, which does not tell a standby from a primary", state)
}

// instanceInRecovery asks the instance whether it is in recovery; when it cannot be reached (it is down) its control data answer (F6-01): an unreachable standby
// is not a promoted one, and reading it as "may have promoted" would leave the old leader held back for ever.
func (m *mapFailover) instanceInRecovery(ctx context.Context, id int) (bool, error) {
	rec, qerr := m.probeInRecovery(ctx, id)
	if qerr == nil {
		return rec, nil
	}
	cd, err := m.controlData(ctx, id)
	if err != nil {
		return false, qerr
	}
	rec, err = recoveryFromControlData(cd["Database cluster state"])
	if err != nil {
		return false, fmt.Errorf("%v; %w", qerr, err)
	}
	return rec, nil
}

// handleMapBootstrap answers the old leader's question: does a bootstrap that names it as the old leader run here, and what is the standby of the map group that
// this server hosts?
func handleMapBootstrap(ctx context.Context, d *haDeps, caller string, body []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	q, err := url.ParseQuery(string(body))
	if err != nil || q.Get("old") == "" || len(q.Get("old")) > 255 {
		return nil, badBody("a query that names the old server")
	}
	if old := q.Get("old"); old != caller {
		return nil, &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "A server asks about the bootstrap that names itself."}
	}
	out := mapBootstrapStatus{Standby: mf.localStandbyState(ctx)}
	if cur, ok := mf.journal.Current(); ok && mf.journal.Active() && cur.OldServer == caller {
		out.Active = true
	}
	return out, nil
}

// localStandbyState is the state of the instances of the map group that are not the primary this server holds.
func (m *mapFailover) localStandbyState(ctx context.Context) string {
	d := m.fd.d
	if d == nil || d.Svc == nil {
		return "none"
	}
	state := "none"
	for _, li := range d.Svc.LocalInstances() {
		if li.Role.Group != 0 {
			continue
		}
		rec, err := m.instanceInRecovery(ctx, li.ID)
		switch {
		case err != nil:
			if state == "none" {
				state = "unknown"
			}
		case rec:
			if state != "primary" {
				state = "in_recovery"
			}
		default:
			state = "primary"
		}
	}
	return state
}

// mapResumeBody is the request of POST /failover/map/resume: Strict asks for the evidence of an operator's call (every server of the cluster must answer).
type mapResumeBody struct {
	Strict bool `json:"strict,omitempty"`
	// Lost is the server that the operator declared lost (P2); the hold is ended without its word. The receiver is held back: its instance, the system
	// database, is stopped, so it cannot read a verifier and cannot check the proof of the operator's session, and a proof must not go to a member that is not the
	// leader. The member that received the operator's request has a live store and has authenticated the session; it attests the declaration (Username, Roles,
	// RequestID, Attestation) with the key of its server certificate, for the receiving server only, and the roles must allow kingres_admin (phase 5 review H1, M-D).
	Lost      string   `json:"lost,omitempty"`
	Username  string   `json:"username,omitempty"`
	Roles     []string `json:"clusterRoles,omitempty"`
	RequestID string   `json:"requestId,omitempty"`
	// Attestation is the signature of the member that received the operator's request over the declaration (fwdproof.Signer.Attest).
	Attestation *fwdproof.Assertion `json:"attestation,omitempty"`
}

// lostMessage is what the attestation of a declaration covers: the kind, the two servers, the user and roles, the operation and the declared server.
func lostMessage(from, to string, b mapResumeBody) fwdproof.Message {
	raw, _ := json.Marshal(struct {
		Lost string `json:"lost"`
	}{b.Lost})
	return internalapi.ForwardRequest{Identity: internalapi.ForwardIdentity{Username: b.Username, ClusterRoles: b.Roles}, OperationID: "abandonMapGroupBootstrap",
		ContentType: "application/json", Body: raw, RequestID: b.RequestID}.ProofMessage(fwdproof.KindMapLost, from, to)
}

// signLost adds this server's attestation of the operator's declaration for the old leader to. The operator's session was authenticated by the request that
// reached this server, and it must hold kingres_admin.
func (m *mapFailover) signLost(id internalapi.ForwardIdentity, b *mapResumeBody, to string) error {
	if m.attest == nil {
		return errors.New("this server cannot sign an attestation")
	}
	if !auth.Allows(auth.Normalize(id.ClusterRoles), auth.RoleAdmin) {
		return errors.New("declaring a server lost needs the role kingres_admin")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	b.Username, b.Roles, b.RequestID = id.Username, id.ClusterRoles, hex.EncodeToString(nonce[:])
	a, err := m.attest(lostMessage(m.self, to, *b))
	if err != nil {
		return err
	}
	b.Attestation = &a
	return nil
}

// openLost checks the attestation of a declaration that a server is lost: the member that sent the call signed exactly these bytes for this server, and the roles it
// names hold kingres_admin. The old leader is held back and has no store to read: nothing here reads a verifier.
func openLost(ctx context.Context, d *haDeps, caller string, b mapResumeBody) error {
	if b.Attestation == nil || b.Username == "" {
		return &internalapi.CallError{Status: http.StatusUnauthorized, Code: "identity_unproven", Message: "The declaration that a server is lost comes with the attestation of the member that received the operator's request."}
	}
	if d.VerifyAttestation == nil {
		return &internalapi.CallError{Status: http.StatusUnauthorized, Code: "identity_unproven", Message: "This server cannot check an attestation."}
	}
	if err := d.VerifyAttestation(ctx, lostMessage(caller, d.ServerHost, b), *b.Attestation); err != nil {
		return &internalapi.CallError{Status: http.StatusUnauthorized, Code: "identity_unproven", Message: "The attestation of the declaration is not valid."}
	}
	if !auth.Allows(auth.Normalize(b.Roles), auth.RoleAdmin) {
		return &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "Declaring a server lost needs the role kingres_admin."}
	}
	return nil
}

// handleMapResume asks this server to resolve the hold of its primary of the map group. The caller is any member: the callee proves what it needs itself.
func handleMapResume(ctx context.Context, d *haDeps, caller string, body []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	var b mapResumeBody
	if len(body) > 0 {
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, badBody("a resume request")
		}
	}
	if b.Lost != "" {
		if err := openLost(ctx, d, caller, b); err != nil {
			return nil, err
		}
	}
	resumed, detail := mf.resolveHeldBack(ctx, b.Strict, b.Lost)
	return mapResumeAnswer{Resumed: resumed, Detail: detail}, nil
}

// candidateOfTheHold is the server that the hand-over of the map group was made to, as the old leader recorded it before it stopped ("" for a hold without a record).
func (m *mapFailover) candidateOfTheHold() string {
	if m.candidateRecord != nil {
		return m.candidateRecord()
	}
	if d := m.fd.d; d != nil && d.LeaseMgr != nil {
		return d.LeaseMgr.Candidate(0)
	}
	return ""
}

// resolveHeldBack ends the hold of this server's primary of the map group when nothing owns it any more. The hold is the guard against reviving a stale primary after a
// hand-over, so its end needs the word of the server that the group was handed to (F7-01):
//
//   - the primary is held back for a hand-over (the marker of a planned switchover);
//   - no server reports an active bootstrap that names this server as the old leader, and none reports a standby of the map group that was promoted;
//   - the candidate that was recorded when the primary was stopped answers, and shows its standby in recovery: an unreachable candidate, or one whose standby cannot be
//     read, blocks (it may be the promoted primary that took writes, and the standby that answers may follow it);
//   - without a record, and for the operator's call (strict), every server of the cluster answers, none of them says "unknown", and at least one shows a standby in
//     recovery: a server that does not answer blocks.
//
// The lease stays the final authority for who may run: the primary starts again only when this server wins it.
func (m *mapFailover) resolveHeldBack(ctx context.Context, strict bool, lost string) (bool, string) {
	if !m.isHeldBack() {
		return false, "the primary of the map group is not held back for a hand-over on this server"
	}
	if lost != "" {
		// P2: the operator declared the server lost. It has to be the one the hold was made for (or the hold has no record), and this server must not hear it.
		if rec := m.candidateOfTheHold(); rec != "" && rec != lost {
			return false, "the hold was made for " + rec + ", not for " + lost
		}
		if m.liveState(ctx, lost) == "live" {
			return false, "the server " + lost + " is heard by this server; it is not lost"
		}
	}
	members, err := m.members(ctx)
	if err != nil {
		return false, "the servers of the cluster cannot be read: " + err.Error()
	}
	cand := m.candidateOfTheHold()
	if lost != "" {
		cand = "" // its word is not asked for; every other server has to answer (below)
	}
	all := strict || cand == ""
	proven, candidateProven := 0, false
	for _, s := range members {
		if s == m.self || (lost != "" && s == lost) {
			continue
		}
		var st mapBootstrapStatus
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := m.call(cctx, s, http.MethodGet, "/failover/map/bootstrap?"+url.Values{"old": {m.self}}.Encode(), nil, &st)
		cancel()
		if err != nil {
			if s == cand {
				return false, "the server " + s + ", to which the map group was handed, does not answer"
			}
			if all {
				return false, "the server " + s + " does not answer"
			}
			continue
		}
		switch {
		case st.Active:
			return false, "a bootstrap that names this server as the old leader is active on " + s
		case st.Standby == "primary":
			return false, "the standby of the map group on " + s + " was promoted"
		case st.Standby == "unknown" && (s == cand || all):
			return false, "the standby of the map group on " + s + " cannot be read"
		case st.Standby == "in_recovery":
			proven++
			if s == cand {
				candidateProven = true
			}
		case s == cand:
			return false, "the server " + s + ", to which the map group was handed, hosts no standby of it"
		}
	}
	if cand != "" && !candidateProven {
		return false, "the server " + cand + ", to which the map group was handed, did not show its standby in recovery"
	}
	if proven == 0 {
		return false, "no standby of the map group could be shown to be in recovery"
	}
	if err := m.endTheHold(ctx); err != nil {
		return false, err.Error()
	}
	m.log.Warn("the primary of the map group was held back for a switchover that nothing owns any more; it is started again")
	return true, ""
}

// heldBackEvery is how often the old leader asks whether the hold of its primary of the map group has an owner.
var heldBackEvery = 10 * time.Second

// WatchHeldBack runs until ctx ends: while the primary of the map group is held back for a hand-over on this server, it resolves the hold when nothing owns it (a
// restarted kingresd finds the marker and no task, and the candidate may be gone or may have given up while this server was away).
func (m *mapFailover) WatchHeldBack(ctx context.Context) {
	t := time.NewTicker(heldBackEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.watchOnce(ctx, time.Now())
	}
}

// heldBackWarnEvery is how often a hold that cannot be resolved is named in the log (F7-03).
const heldBackWarnEvery = time.Minute

// watchOnce is one look at the hold: a hold that is resolved or gone is forgotten, one that cannot be resolved is remembered for the failover status and named in the
// log at warning level once a minute, with the reason and the call that an operator can make.
func (m *mapFailover) watchOnce(ctx context.Context, now time.Time) {
	if !m.isHeldBack() {
		m.setHeldWhy("", now)
		return
	}
	if ok, why := m.resolveHeldBack(ctx, false, ""); ok {
		m.setHeldWhy("", now)
	} else {
		m.setHeldWhy(why, now)
	}
}

func (m *mapFailover) setHeldWhy(why string, now time.Time) {
	m.mu.Lock()
	m.heldWhy = why
	warn := why != "" && now.Sub(m.heldWarned) >= heldBackWarnEvery
	if warn {
		m.heldWarned = now
	}
	m.mu.Unlock()
	if warn {
		m.log.Warn("the primary of the map group is held back for a switchover that nothing owns, and it cannot be started again yet; abandonMapGroupBootstrap asks every server to try again", "why", why)
	}
}

// heldStatus is the failover status of a hold that nothing owns and that cannot be resolved (F7-03): the system database is the held-back primary, so no alert can be
// recorded, and the status is what an operator can read.
func (m *mapFailover) heldStatus() (failover.Status, bool) {
	m.mu.Lock()
	why := m.heldWhy
	m.mu.Unlock()
	if why == "" || !m.isHeldBack() {
		return failover.Status{}, false
	}
	reason := "The primary of the map group is held back for a switchover that nothing owns, and it cannot be started again yet: " + why + ". abandonMapGroupBootstrap asks every server to try again."
	return failover.Status{State: "paused", PausedReason: &reason, NeedsDecision: []failover.Blocker{}, Groups: []failover.GroupProgress{}}, true
}

// resumeOldLeaderOf asks the old leader's server to resolve its hold (a planned bootstrap that was given up): best effort; the loop of that server does it as well.
func (m *mapFailover) resumeOldLeaderOf(ctx context.Context, oldServer string) (bool, error) {
	var out mapResumeAnswer
	if err := m.call(ctx, oldServer, http.MethodPost, "/failover/map/resume", mapResumeBody{}, &out); err != nil {
		return false, err
	}
	if !out.Resumed && out.Detail != "" {
		return false, errors.New(out.Detail)
	}
	return out.Resumed, nil
}

// queryInRecovery asks the running instance, by a connection to it.
func (m *mapFailover) queryInRecovery(ctx context.Context, id int) (bool, error) {
	c, err := m.svc.InstanceConn(ctx, id, sysdb.Database)
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var rec bool
	if err := c.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&rec); err != nil {
		return false, err
	}
	return rec, nil
}

// call is a call to another server (a test stands in for the network).
func (m *mapFailover) call(ctx context.Context, server, method, path string, in, out any) error {
	if m.callPeer != nil {
		return m.callPeer(ctx, server, method, path, in, out)
	}
	return m.peers.do(ctx, server, method, path, in, out)
}

// members lists the servers of the cluster (a test stands in for the catalog and the peer directory).
func (m *mapFailover) members(ctx context.Context) ([]string, error) {
	if m.memberList != nil {
		return m.memberList(ctx)
	}
	return (&mapPorts{m: m}).Members(ctx)
}

// isHeldBack says that the primary of the map group is held back on this server for a hand-over.
func (m *mapFailover) isHeldBack() bool {
	if m.heldBack != nil {
		return m.heldBack()
	}
	d := m.fd.d
	return d != nil && d.LeaseMgr != nil && d.LeaseMgr.HandOverMarked(0)
}

// endTheHold ends the hold of the primary: the same two calls as the rollback of a switchover. The hand-over marker ends and the primary starts when the lease is
// won; the hold rules of the writes end.
func (m *mapFailover) endTheHold(ctx context.Context) error {
	if m.endHold != nil {
		return m.endHold(ctx)
	}
	if err := m.peers.Resume(ctx, m.self, failover.ResumeRequest{Group: 0, ExpectedTerm: -1}); err != nil {
		return fmt.Errorf("the hold of the primary could not be ended: %w", err)
	}
	if err := m.peers.Unquiesce(ctx, m.self, failover.QuiesceRequest{Group: 0, ExpectedTerm: -1}); err != nil {
		m.log.Warn("the writes of the map group could not be let in again after its hold was ended", "error", err)
	}
	return nil
}

// testInstanceUnreachable is set by the fault "promote:unreachable": the question whether the standby is in recovery then fails as if its instance were down.
var testInstanceUnreachable atomic.Bool

func (m *mapFailover) probeInRecovery(ctx context.Context, id int) (bool, error) {
	if testInstanceUnreachable.Load() {
		return false, errors.New("the instance cannot be reached (injected)")
	}
	if m.queryInRec != nil {
		return m.queryInRec(ctx, id)
	}
	return m.queryInRecovery(ctx, id)
}

func (m *mapFailover) controlData(ctx context.Context, id int) (map[string]string, error) {
	if m.controlDataOf != nil {
		return m.controlDataOf(ctx, id)
	}
	return m.svc.Manager().ControlData(ctx, id)
}

// mapHeldAnswer says whether this server holds back its primary of the map group for a hand-over.
type mapHeldAnswer struct {
	Held bool `json:"held"`
}

// handleMapHeld answers whether the primary of the map group is held back on this server. The operator's declaration that a server is lost is sent to the
// server that says so and to no other (phase 5 review M-D). It reveals one bit that the failover status of that server shows anyway.
func handleMapHeld(_ context.Context, d *haDeps, _ string, _ []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	return mapHeldAnswer{Held: mf.isHeldBack()}, nil
}
