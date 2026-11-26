// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package membership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// The escrow of the key of the certificate authority (ha-phase5-part2.md, section 3.4.1): the leader seals the CA key and
// the revocation list to each server that may become leader and sends each its copy; a new leader opens its copy and
// goes on (pki.AdoptEscrow). The cryptography is package pki; this file is the leader's side: who gets a copy, when it is
// sent again, who is told to delete it, what a leader that takes over (or comes back) does first, and the operations of
// the operator.

// EscrowRefresh is how old a copy may get before the leader seals and sends it again (it also repairs a server that lost
// its file). A copy is sent at once when the revocation list or the set of recipients changes.
const EscrowRefresh = 6 * time.Hour

// escrowDropAfterMisses is how many rounds in a row an active member may be missing from the candidates (a standby that is
// being rebuilt, a read that came back empty) before its copy is deleted. A server that is no longer an active member loses
// its copy at once.
const escrowDropAfterMisses = 3

const escrowCallTimeout = 20 * time.Second

// escrowDropTimeout bounds one attempt to delete a copy: a server that is gone must not hold the round (and the lock of the escrow) for long.
const escrowDropTimeout = 5 * time.Second

// maxCounter bounds the epoch and the generation read from the system database or from a server (no overflow at the next increment).
const maxCounter = int64(1) << 62

// maxHolders bounds the holders read from the system database.
const maxHolders = 64

// escrowGuard serializes the escrow files of this server (the bookkeeping, the keys and the copy).
type escrowGuard struct {
	mu    sync.Mutex
	recMu sync.Mutex // guards escrowRecipients only (read while mu is held)
	// mirrored is the last content written to the system database.
	mirrored string
}

// EscrowRecipients returns the ids of the servers that may become leader, from the standbys of the map group. Nil means
// every active member other than this server.
type EscrowRecipients func(ctx context.Context) ([]string, error)

// escrowShared is what the escrow keeps in the system database (kingres_cp.cluster_setting, key caEscrow), which follows the
// map group to the new leader: the operator's choice, the order of the copies and the servers that hold one. The sealed
// copies carry none of it, so a new leader learns from here that the operator had switched the escrow off.
type escrowShared struct {
	Enabled    bool     `json:"enabled"`
	Generation int64    `json:"generation"`
	Epoch      int64    `json:"epoch"`
	Holders    []string `json:"holders"`
}

const escrowSettingKey = "caEscrow"

// escrowDB is the system database side of the escrow (replaced in the tests).
type escrowDB interface {
	Load(ctx context.Context) (escrowShared, bool, error)
	Save(ctx context.Context, s escrowShared) error
}

type sysEscrowDB struct{ m *Manager }

func (d sysEscrowDB) Load(ctx context.Context) (out escrowShared, found bool, err error) {
	err = d.m.sys(ctx, func(c *pgx.Conn) error {
		var raw []byte
		err := c.QueryRow(ctx, `SELECT value FROM kingres_cp.cluster_setting WHERE key = $1`, escrowSettingKey).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return json.Unmarshal(raw, &out)
	})
	return out, found, err
}

func (d sysEscrowDB) Save(ctx context.Context, s escrowShared) error {
	j, _ := json.Marshal(s)
	return d.m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, escrowSettingKey, j)
		return err
	})
}

func (m *Manager) escrowStore() escrowDB {
	if m.escrowDB != nil {
		return m.escrowDB
	}
	return sysEscrowDB{m}
}

// loadEscrowState reads the bookkeeping file.
func (m *Manager) loadEscrowState() (pki.EscrowState, error) {
	return pki.LoadEscrowState(m.Svc.PKIDir())
}

// saveEscrowState writes the bookkeeping file and mirrors the part that a new leader needs to the system database (a
// failure to write it is logged; the next save tries again).
func (m *Manager) saveEscrowState(ctx context.Context, st pki.EscrowState) error {
	if err := pki.SaveEscrowState(m.Svc.PKIDir(), st); err != nil {
		return err
	}
	m.mirrorEscrowState(ctx, st)
	return nil
}

// mirrorEscrowState writes the part of the state that a new leader needs to the system database when it changed.
func (m *Manager) mirrorEscrowState(ctx context.Context, st pki.EscrowState) {
	sh := escrowShared{Enabled: st.Enabled, Generation: st.Generation, Epoch: st.Epoch, Holders: st.SortedRecipients()}
	j, _ := json.Marshal(sh)
	if string(j) == m.escrow.mirrored {
		return
	}
	mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.escrowStore().Save(mctx, sh); err != nil {
		m.log().Warn("the state of the CA key escrow could not be written to the system database", "error", err)
		return
	}
	m.escrow.mirrored = string(j)
}

// SetEscrowRecipients sets where the recipients come from (kingresd sets it to the servers that host a ready standby of
// the map group).
func (m *Manager) SetEscrowRecipients(f EscrowRecipients) {
	m.escrow.recMu.Lock()
	defer m.escrow.recMu.Unlock()
	m.escrowRecipients = f
}

// validServerID says whether a string can be the id of a server (a host name or an address).
func validServerID(id string) bool {
	if id == "" || len(id) > 253 {
		return false
	}
	for _, c := range id {
		if c <= 32 || c == 127 || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

// escrowActiveServers lists the active members other than this server whose certificates are not revoked.
func (m *Manager) escrowActiveServers(ctx context.Context) ([]string, error) {
	if m.escrowActive != nil {
		return m.escrowActive(ctx)
	}
	rows, err := m.listServers(ctx)
	if err != nil {
		return nil, err
	}
	var active []string
	for _, r := range rows {
		if r.Status == StatusActive && r.ID != m.Self && r.RevokedAt == nil {
			active = append(active, r.ID)
		}
	}
	return active, nil
}

// peerCall is how the leader reaches a server for the escrow and the revocation lists: the peer client (a revoked or
// removed server is reached unchecked). Tests replace it with the receivers of other servers.
func (m *Manager) peerCall(ctx context.Context, id, method, path string, in, out any, unchecked bool) error {
	if m.escrowTransport != nil {
		return m.escrowTransport(ctx, id, method, path, in, out)
	}
	if unchecked {
		return m.doUnchecked(ctx, id, method, path, in, out)
	}
	return m.do(ctx, id, method, path, in, out)
}

// escrowCandidates returns the servers that should hold a copy and the active members.
func (m *Manager) escrowCandidates(ctx context.Context) (cands, active []string, err error) {
	active, err = m.escrowActiveServers(ctx)
	if err != nil {
		return nil, nil, err
	}
	m.escrow.recMu.Lock()
	f := m.escrowRecipients
	m.escrow.recMu.Unlock()
	if f == nil {
		out := append([]string(nil), active...)
		slices.Sort(out)
		return out, active, nil
	}
	want, err := f(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range want {
		// A server that is not an active member (removed, leaving, revoked) never gets a copy.
		if slices.Contains(active, id) && !slices.Contains(cands, id) {
			cands = append(cands, id)
		}
	}
	slices.Sort(cands)
	return cands, active, nil
}

// Limits on what a peer can make the leader merge.
const (
	maxSerialsFromPeer = 10000
	maxSerialLength    = 64
	// maxCRLJump is how far above its own clock a revocation list number may be before the leader refuses to follow it.
	maxCRLJump = 24 * time.Hour
)

// issuedSerials is the set of serial numbers this CA issued (the certificate table and the certificates of the servers).
func (m *Manager) issuedSerials(ctx context.Context) (map[string]bool, error) {
	if m.escrowIssued != nil {
		return m.escrowIssued(ctx)
	}
	out := map[string]bool{}
	err := m.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT serial FROM kingres_cp.certificate UNION SELECT certificate_serial FROM kingres_cp.server WHERE certificate_serial IS NOT NULL`)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, s := range list {
			out[strings.ToLower(s)] = true
		}
		return err
	})
	return out, err
}

// readablePeers are the servers whose answers a leader trusts to read: active members whose certificates are not revoked (a revoked or
// leaving server is the one that has a reason to lie).
func (m *Manager) readablePeers(ctx context.Context) ([]string, error) {
	if m.escrowReadable != nil {
		return m.escrowReadable(ctx)
	}
	if m.escrowActive != nil {
		return m.escrowActive(ctx)
	}
	rows, err := m.listServers(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if r.Status == StatusActive && r.RevokedAt == nil && r.ID != m.Self {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

// learnCRLNumber raises the number of the next revocation list above one that a server holds. A number far above the clock is not followed
// (a server with a wrong date would pin every later list above it).
func (m *Manager) learnCRLNumber(ca *pki.CA, from string, n int64) {
	if n <= 0 {
		return
	}
	if n > time.Now().Add(maxCRLJump).Unix() {
		m.log().Warn("a server holds a revocation list whose number is more than a day above this server's clock; not following it", "server", from, "number", n)
		return
	}
	if err := ca.EnsureCRLNumberAbove(n); err != nil {
		m.log().Error("raising the number of the revocation list failed", "error", err)
	}
}

// reconcileRevocations merges every revocation this server can learn of into the CA it holds: the file the previous leader
// distributed, the copy of the revocation list in its sealed copy and the revocation list file that is published (all three are this
// server's own, and sealed or signed), and, from the other servers, the serials of certificates this CA issued. A revocation list never
// shrinks, so a leader that was away (or a new one) does this before it distributes anything: its list must be at least what the servers
// already trust. What a server reports that this CA never issued is ignored, whoever the server is: the merge cannot be undone.
func (m *Manager) reconcileRevocations(ctx context.Context, ca *pki.CA) {
	dir := m.Svc.PKIDir()
	now := time.Now()
	add := func(from string, serials []string) {
		if n, err := ca.MergeRevoked(serials, now); err != nil {
			m.log().Error("merging revocations into the CA failed", "from", from, "error", err)
		} else if n > 0 {
			m.log().Warn("revocations that this server did not have were merged before it distributes the list", "from", from, "count", n)
		}
	}
	if l, err := pki.LoadRevokedList(dir); err == nil {
		add("revoked-serials.json", l)
	}
	if l, err := pki.HeldRevoked(dir, m.Self); err == nil {
		add("the sealed copy", l)
	}
	if b, err := pki.ReadFile(dir, pki.CRLFile); err == nil {
		if n, s, err := pki.CRLContent(b); err == nil {
			list := make([]string, 0, len(s))
			for x := range s {
				list = append(list, x)
			}
			add(pki.CRLFile, list)
			if err := ca.EnsureCRLNumberAbove(n); err != nil {
				m.log().Error("raising the number of the revocation list failed", "error", err)
			}
		}
	}
	ids, err := m.readablePeers(ctx)
	if err != nil {
		m.log().Warn("the servers could not be listed to learn their revocation lists", "error", err)
		return
	}
	issued, ierr := m.issuedSerials(ctx)
	if ierr != nil {
		m.log().Warn("the serials this CA issued could not be read; the revocation lists of the servers are not merged", "error", ierr)
	}
	for _, id := range ids {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var out struct {
			Serials   []string `json:"serials"`
			CRLNumber int64    `json:"crlNumber"`
		}
		err := m.peerCall(cctx, id, "GET", "/revoked", nil, &out, true)
		cancel()
		if err != nil {
			m.log().Warn("the revocation list of a server could not be read", "server", id, "error", err)
			continue
		}
		m.learnCRLNumber(ca, id, out.CRLNumber)
		if ierr != nil {
			continue
		}
		var take []string
		ignored := 0
		for i, s := range out.Serials {
			s = strings.ToLower(s)
			switch {
			case i >= maxSerialsFromPeer, len(s) > maxSerialLength, !issued[s]:
				ignored++
			default:
				take = append(take, s)
			}
		}
		if ignored > 0 {
			m.log().Warn("a server reported revoked serials that this CA did not issue (or too many); they are ignored", "server", id, "count", ignored)
		}
		add(id, take)
	}
}

// installRevocations sends the revocation list to one server. A server that holds a revocation the list lacks, or a higher number,
// refuses it (stale_revocations, with the number it holds); the leader then learns what the servers hold, issues the list again above
// that number and sends it once more.
func (m *Manager) installRevocations(ctx context.Context, id string, ca *pki.CA, req wire.PKIRequest) error {
	err := m.peerCall(ctx, id, "POST", "/pki/install", req, nil, true)
	var ae *peerclient.APIError
	if !errors.As(err, &ae) || ae.Code != codeStaleRevocations {
		return err
	}
	m.log().Warn("a server refused the revocation list because it takes a revocation back; merging and sending again", "server", id)
	if n, ok := pki.ParseHeldNumber(ae.Message); ok {
		m.learnCRLNumber(ca, id, n)
	}
	m.reconcileRevocations(ctx, ca)
	if err := m.Svc.PublishLocalCRL(ctx); err != nil {
		return err
	}
	crl, err := m.Svc.CRLPEM()
	if err != nil {
		return err
	}
	req.Revoked, req.CRL = ca.Revoked(), string(crl)
	return m.peerCall(ctx, id, "POST", "/pki/install", req, nil, true)
}

// distributeAll sends to every server in the order given and does not stop at the first refusal: the errors are collected, the servers
// that failed are tried once more, and the result is the errors that remain. A server in ignore (one whose certificates are revoked)
// has no say: its failure is logged and does not fail the whole.
func (m *Manager) distributeAll(ids []string, ignore map[string]bool, send func(id string) error) error {
	var failed []string
	for _, id := range ids {
		if err := send(id); err != nil {
			if ignore[id] {
				m.log().Warn("the revocation list could not be sent to a server whose certificates are revoked", "server", id, "error", err)
				continue
			}
			m.log().Warn("the revocation list could not be sent to a server; the others go on", "server", id, "error", err)
			failed = append(failed, id)
		}
	}
	var errs []error
	for _, id := range failed {
		if err := send(id); err != nil {
			errs = append(errs, fmt.Errorf("sending the revocation list to %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// revocationTargets are the servers that get a revocation list: the active members, those whose certificates are revoked last (they
// accept the leader's certificate but must not hold the others up), and the set of the revoked ones.
func (m *Manager) revocationTargets(ctx context.Context) (ids []string, revoked map[string]bool, err error) {
	revoked = map[string]bool{}
	if m.escrowActive != nil {
		ids, err = m.escrowActive(ctx)
		if err != nil {
			return nil, nil, err
		}
		if m.escrowRevoked != nil {
			if revoked, err = m.escrowRevoked(ctx); err != nil {
				return nil, nil, err
			}
		}
	} else {
		rows, lerr := m.listServers(ctx)
		if lerr != nil {
			return nil, nil, lerr
		}
		for _, r := range rows {
			if r.Status == StatusActive && r.ID != m.Self {
				ids = append(ids, r.ID)
				if r.RevokedAt != nil {
					revoked[r.ID] = true
				}
			}
		}
	}
	ids = slices.Clone(ids)
	slices.SortStableFunc(ids, func(a, b string) int {
		switch {
		case revoked[a] == revoked[b]:
			return 0
		case revoked[a]:
			return 1
		}
		return -1
	})
	return ids, revoked, nil
}

// AdoptEscrow is called on a server that has become the manager leader (step B8 of the failover of the map group). When it
// holds no usable CA key it makes one from its sealed copy; when it holds one (a former leader that is back) it keeps it.
// Either way it merges every revocation it can learn of before it distributes anything, takes a new epoch (so that copies
// sealed by the leader it replaces are refused) and goes on with the escrow as the operator left it. It returns whether the
// server holds the CA key afterwards.
func (m *Manager) AdoptEscrow(ctx context.Context) (bool, error) {
	m.escrow.mu.Lock()
	defer m.escrow.mu.Unlock()
	dir := m.Svc.PKIDir()
	pki.CleanTempFiles(dir, 5*time.Minute, 3)
	res, err := pki.AdoptEscrow(dir, m.Self, time.Now())
	switch {
	case err != nil:
		m.log().Error("the sealed copy of the CA key could not be adopted", "error", err)
		return false, err
	case res == pki.AdoptNone:
		return false, nil
	case res == pki.Adopted:
		m.log().Info("this server adopted the CA key from its sealed copy; certificates can be issued and revoked here")
	}
	ca, err := m.Svc.CA()
	if err != nil {
		return false, err
	}
	m.reconcileRevocations(ctx, ca)

	st, err := m.loadEscrowState()
	if err != nil {
		return true, err
	}
	known := st.Enabled || len(st.Recipients) > 0 || st.Epoch > 0
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sh, found, lerr := m.escrowStore().Load(lctx)
	cancel()
	if lerr != nil {
		m.log().Warn("the state of the CA key escrow could not be read from the system database", "error", lerr)
	}
	held, herr := pki.HeldEscrow(dir)
	if found {
		known = true
		st.Enabled = sh.Enabled
		// The row is read from a replica and can be wrong or hostile: the counters are bounded (no overflow at the next increment), the holders
		// are valid server ids and few.
		st.Generation = max(st.Generation, min(max(sh.Generation, 0), maxCounter))
		st.Epoch = max(st.Epoch, min(max(sh.Epoch, 0), maxCounter))
		for i, id := range sh.Holders {
			if i >= maxHolders || !validServerID(id) {
				m.log().Warn("a holder in the state of the CA key escrow was ignored", "holder", fmt.Sprintf("%.40q", id))
				continue
			}
			if _, ok := st.Recipients[id]; !ok {
				st.Recipients[id] = pki.EscrowRecipientState{}
			}
		}
	} else if herr == nil {
		// No record of the operator's choice, but there is a copy: the escrow was on.
		known = true
		st.Enabled = true
	}
	if herr == nil {
		st.Generation = max(st.Generation, min(held.Position.Generation, maxCounter))
		st.Epoch = max(st.Epoch, min(held.Position.Epoch, maxCounter))
	}
	if !known {
		return true, nil
	}
	// A new leader takes a term of its own and a generation above every one it has seen, so that nothing sealed by the leader it
	// replaces can win, and sends every recipient its copy again.
	st.Epoch++
	st.Generation++
	st.Seq = 0
	st.Swept = false
	for id, rs := range st.Recipients {
		rs.Generation, rs.Epoch = 0, 0
		st.Recipients[id] = rs
	}
	return true, m.saveEscrowState(ctx, st)
}

func (m *Manager) escrowStatusLocked(st pki.EscrowState, ca *pki.CA) api.CaEscrowStatus {
	out := api.CaEscrowStatus{Enabled: st.Enabled, HoldsKey: ca != nil, Generation: st.Generation, Recipients: []api.CaEscrowRecipient{}}
	hash := ""
	if ca != nil {
		hash, _ = ca.EscrowContentHash()
	}
	complete := st.Enabled && len(st.Recipients) > 0
	for _, id := range st.SortedRecipients() {
		rs := st.Recipients[id]
		r := api.CaEscrowRecipient{ServerID: id, State: "pending"}
		if rs.KeyID != "" {
			k, g, t := rs.KeyID, rs.Generation, rs.SealedAt
			r.KeyID, r.Generation, r.SealedAt = &k, &g, &t
		}
		switch {
		case rs.DropPending:
			r.State = "dropPending"
		case rs.LastError != "" && rs.LastRefused:
			r.State = "refused"
		case rs.LastError == "" && rs.KeyID != "" && rs.Generation == st.Generation && rs.Epoch == st.Epoch && (hash == "" || rs.ContentHash == hash):
			r.State = "current"
		}
		if rs.LastError != "" {
			e := rs.LastError
			r.Error = &e
		}
		if r.State != "current" {
			complete = false
		}
		out.Recipients = append(out.Recipients, r)
	}
	out.Complete = complete
	switch {
	case !st.Enabled && len(st.Recipients) > 0:
		out.Message = "Escrow is off, but some servers have not yet deleted their copy; the leader asks them again every minute."
	case !st.Enabled:
		out.Message = "Escrow is off: a new manager leader has no CA key until an operator restores it by hand."
	case ca == nil:
		out.Message = "This server holds no CA key; ask the manager leader."
	case len(st.Recipients) == 0:
		out.Message = "No server hosts a ready standby of the map group yet, so no copy of the CA key exists outside the leader."
	case !complete:
		out.Message = "Some recipients do not hold the newest copy yet, or a server that no longer qualifies has not deleted its copy; the leader tries again every minute."
	default:
		out.Message = "Every server that can become leader holds a sealed copy of the CA key."
	}
	return out
}

// EscrowStatus describes the escrow as the leader sees it.
func (m *Manager) EscrowStatus(ctx context.Context) (api.CaEscrowStatus, error) {
	m.escrow.mu.Lock()
	defer m.escrow.mu.Unlock()
	return m.escrowStatusNow()
}

func (m *Manager) escrowStatusNow() (api.CaEscrowStatus, error) {
	st, err := m.loadEscrowState()
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	ca, err := m.Svc.CA()
	if err != nil {
		ca = nil
	}
	return m.escrowStatusLocked(st, ca), nil
}

// SetEscrow switches the escrow on or off (setCaEscrow) and sends the copies, or the orders to delete them, now.
func (m *Manager) SetEscrow(ctx context.Context, enabled bool) (api.CaEscrowStatus, error) {
	m.escrow.mu.Lock()
	defer m.escrow.mu.Unlock()
	ca, err := m.ca()
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	st, err := m.loadEscrowState()
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	if st.Epoch == 0 {
		st.Epoch = 1
	}
	if enabled && !st.Enabled {
		st.Generation++
	}
	st.Enabled, st.Swept = enabled, false
	if err := m.saveEscrowState(ctx, st); err != nil {
		return api.CaEscrowStatus{}, err
	}
	if err := m.syncEscrowLocked(ctx, ca, false); err != nil {
		return api.CaEscrowStatus{}, err
	}
	return m.escrowStatusNow()
}

// RotateEscrow makes every recipient create a new escrow key and seals a new generation of the copy to it; the old keys
// and copies are removed as each new copy is stored (rotateCaEscrow).
func (m *Manager) RotateEscrow(ctx context.Context) (api.CaEscrowStatus, error) {
	m.escrow.mu.Lock()
	defer m.escrow.mu.Unlock()
	ca, err := m.ca()
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	st, err := m.loadEscrowState()
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	if !st.Enabled {
		return api.CaEscrowStatus{}, problem.New(problem.Conflict, "The escrow of the CA key is off; switch it on with setCaEscrow first.")
	}
	st.Generation++
	if err := m.saveEscrowState(ctx, st); err != nil {
		return api.CaEscrowStatus{}, err
	}
	if err := m.syncEscrowLocked(ctx, ca, true); err != nil {
		return api.CaEscrowStatus{}, err
	}
	return m.escrowStatusNow()
}

// SyncEscrow is the periodic round on the leader: it sends the copies that are missing or old and takes them away from
// servers that are no longer recipients. It does nothing on a server that is not the leader or holds no CA key.
func (m *Manager) SyncEscrow(ctx context.Context) error {
	if m.Leaders != nil {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		id, err := m.Leaders.Get(lctx)
		cancel()
		if err != nil || id != m.Self {
			return nil
		}
	}
	ca, err := m.Svc.CA()
	if err != nil {
		return nil // no CA key here: nothing to send (a new leader adopts its copy first)
	}
	m.escrow.mu.Lock()
	defer m.escrow.mu.Unlock()
	return m.syncEscrowLocked(ctx, ca, false)
}

// RunEscrow runs SyncEscrow every interval until ctx ends.
func (m *Manager) RunEscrow(ctx context.Context, interval time.Duration) {
	pki.CleanTempFiles(m.Svc.PKIDir(), 5*time.Minute, 3)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := m.SyncEscrow(ctx); err != nil && ctx.Err() == nil {
			m.log().Warn("sending the sealed copies of the CA key", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// dropDue counts the rounds of a record whose copy could not be deleted: after 30 failed rounds the server is tried only every tenth round
// (a server that is gone for good must not cost a timeout in every round).
func dropDue(rs *pki.EscrowRecipientState) bool {
	rs.Misses++
	return rs.Misses < 30 || rs.Misses%10 == 0
}

// markDropFailed keeps the record of a server whose copy could not be deleted, so that the drop is tried again.
func (m *Manager) markDropFailed(st *pki.EscrowState, id string, err error) {
	rs := st.Recipients[id]
	if !rs.DropPending {
		m.log().Warn("the sealed copy of the CA key on a server could not be deleted; it is tried again every round, and until it succeeds "+
			"that server must be treated as knowing the CA key", "server", id, "error", err)
	}
	rs.DropPending, rs.LastError, rs.LastRefused = true, err.Error(), false
	st.Recipients[id] = rs
}

// syncEscrowLocked reconciles the recipients with the standbys. With rotate every recipient makes a new escrow key.
func (m *Manager) syncEscrowLocked(ctx context.Context, ca *pki.CA, rotate bool) error {
	st, err := m.loadEscrowState()
	if err != nil {
		return err
	}
	if !st.Enabled {
		return m.sweepEscrowLocked(ctx, st)
	}
	cands, active, err := m.escrowCandidates(ctx)
	if err != nil {
		return fmt.Errorf("working out which servers may become leader: %w", err)
	}
	for _, id := range st.SortedRecipients() {
		if slices.Contains(cands, id) {
			continue
		}
		rs := st.Recipients[id]
		// An active member that is only missing from the candidates (its standby is being rebuilt) keeps its copy for a few rounds;
		// a server that is no longer a member loses it now. A failed drop keeps the record: the drop is repeated every round.
		if slices.Contains(active, id) && !rs.DropPending {
			rs.Misses++
			st.Recipients[id] = rs
			if rs.Misses < escrowDropAfterMisses {
				continue
			}
		}
		if rs.DropPending && !dropDue(&rs) {
			st.Recipients[id] = rs
			continue
		}
		if err := m.dropEscrowAt(ctx, id, st.Epoch); err != nil {
			m.markDropFailed(&st, id, err)
			continue
		}
		delete(st.Recipients, id)
	}
	hash, err := ca.EscrowContentHash()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, id := range cands {
		rs := st.Recipients[id]
		rs.Misses, rs.DropPending = 0, false
		fresh := rs.KeyID != "" && rs.LastError == "" && rs.Generation == st.Generation && rs.Epoch == st.Epoch && rs.ContentHash == hash && now.Sub(rs.SealedAt) < EscrowRefresh
		if fresh && !rotate {
			st.Recipients[id] = rs
			continue
		}
		rs.LastTry = now
		if err := m.sendEscrow(ctx, ca, id, &st, rotate, hash, &rs); err != nil {
			rs.LastError = err.Error()
			var ae *peerclient.APIError
			rs.LastRefused = errors.As(err, &ae)
			m.log().Warn("sealed copy of the CA key not delivered", "server", id, "error", err)
		}
		st.Recipients[id] = rs
	}
	return m.saveEscrowState(ctx, st)
}

// sweepEscrowLocked is the round of a leader whose escrow is off: it tells every recorded recipient and every active member to
// delete a copy (the call is idempotent), keeps the record of each server that could not be told, and goes quiet once all were.
func (m *Manager) sweepEscrowLocked(ctx context.Context, st pki.EscrowState) error {
	// A cluster that never used the escrow has nothing to delete; one that did (it has an epoch) asks every member once.
	if (st.Swept || st.Epoch == 0) && len(st.Recipients) == 0 {
		m.mirrorEscrowState(ctx, st)
		return nil
	}
	active, err := m.escrowActiveServers(ctx)
	if err != nil {
		return err
	}
	targets := slices.Clone(st.SortedRecipients())
	for _, id := range active {
		if !slices.Contains(targets, id) {
			targets = append(targets, id)
		}
	}
	for _, id := range targets {
		if err := m.dropEscrowAt(ctx, id, st.Epoch); err != nil {
			m.markDropFailed(&st, id, err)
			continue
		}
		delete(st.Recipients, id)
	}
	st.Swept = len(st.Recipients) == 0
	return m.saveEscrowState(ctx, st)
}

func (m *Manager) sendEscrow(ctx context.Context, ca *pki.CA, id string, st *pki.EscrowState, rotate bool, hash string, rs *pki.EscrowRecipientState) error {
	octx, cancel := context.WithTimeout(ctx, escrowCallTimeout)
	defer cancel()
	var offer wire.EscrowOfferResponse
	if err := m.peerCall(octx, id, "POST", "/pki/escrow/offer", wire.EscrowOfferRequest{Rotate: rotate, Epoch: st.Epoch}, &offer, false); err != nil {
		return fmt.Errorf("asking for the escrow key: %w", err)
	}
	pub, err := pki.DecodeKey(offer.PublicKey)
	if err != nil {
		return fmt.Errorf("the escrow key of %s is not valid", id)
	}
	// The recipient holds a copy of a leader of a newer epoch than this one: this server is a former leader (or lost its record). It must
	// not override the newer leader, so it seals nothing; only the operator's rotation, made on the leader of the moment, takes a higher epoch.
	if offer.HeldEpoch < 0 || offer.HeldEpoch > maxCounter || offer.HeldGeneration < 0 || offer.HeldGeneration > maxCounter {
		return fmt.Errorf("the server %s reports a position of its copy that is not valid", id)
	}
	if offer.HeldEpoch > st.Epoch {
		// Not even the operator's rotation takes an epoch above a server's: that would make the real leader the one that is refused. The leader of
		// a newer epoch seals; this one does not.
		return fmt.Errorf("the server holds a copy of epoch %d, above this leader's %d: a newer leader has sealed it", offer.HeldEpoch, st.Epoch)
	}
	st.Seq++
	now := time.Now()
	blob, err := pki.SealCA(ca, id, offer.KeyID, pub, pki.Position{Epoch: st.Epoch, Generation: st.Generation, Seq: st.Seq}, now)
	if err != nil {
		return err
	}
	if err := m.peerCall(octx, id, "POST", "/pki/escrow/store", wire.EscrowStoreRequest{Blob: blob}, nil, false); err != nil {
		return fmt.Errorf("storing the copy: %w", err)
	}
	*rs = pki.EscrowRecipientState{KeyID: blob.KeyID, Generation: st.Generation, Epoch: st.Epoch, SealedAt: blob.SealedAt, ContentHash: hash, LastTry: now}
	return nil
}

// dropEscrowAt tells a server to delete its copy and escrow keys. A revoked server is told as well.
func (m *Manager) dropEscrowAt(ctx context.Context, id string, epoch int64) error {
	if id == m.Self {
		// The leader holds the key itself; a copy it kept from the time it was a standby is deleted directly (the escrow lock is held).
		return pki.DropEscrow(m.Svc.PKIDir())
	}
	octx, cancel := context.WithTimeout(ctx, escrowCallTimeout)
	defer cancel()
	return m.peerCall(octx, id, "POST", "/pki/escrow/drop", wire.EscrowDropRequest{Epoch: epoch}, nil, true)
}

// ----------------------------------------------- receiving side (any server)

// handleEscrowCall answers the escrow calls of the internal API. Only the manager leader may make them (the table of
// internalapi.Calls); what is stored is checked again here, and a call from a leader whose epoch is older than the copy held
// is refused.
func (r *Receiver) handleEscrowCall(_ context.Context, key string, body []byte) (any, bool, error) {
	m := r.M
	dir := m.Svc.PKIDir()
	stale := func(epoch int64) error {
		if h, err := pki.HeldEscrow(dir); err == nil && epoch < h.Position.Epoch {
			return &internalapi.CallError{Status: http.StatusConflict, Code: "stale_epoch", Message: "A leader of a newer epoch has sealed a copy for this server."}
		}
		return nil
	}
	switch key {
	case "POST /pki/escrow/offer":
		req, err := decode[wire.EscrowOfferRequest](body)
		if err != nil {
			return nil, true, err
		}
		m.escrow.mu.Lock()
		defer m.escrow.mu.Unlock()
		if req.Rotate {
			if err := stale(req.Epoch); err != nil {
				return nil, true, err
			}
		}
		id, pub, err := pki.RecipientOffer(dir, req.Rotate, time.Now())
		if err != nil {
			return nil, true, err
		}
		out := wire.EscrowOfferResponse{KeyID: id, PublicKey: pki.EncodeKey(pub)}
		if h, err := pki.HeldEscrow(dir); err == nil {
			out.HeldEpoch, out.HeldGeneration = h.Position.Epoch, h.Position.Generation
		}
		return out, true, nil
	case "POST /pki/escrow/store":
		req, err := decode[wire.EscrowStoreRequest](body)
		if err != nil {
			return nil, true, err
		}
		m.escrow.mu.Lock()
		err = pki.StoreEscrow(dir, m.Self, req.Blob)
		m.escrow.mu.Unlock()
		if errors.Is(err, pki.ErrEscrowRefused) {
			return nil, true, &internalapi.CallError{Status: http.StatusUnprocessableEntity, Code: "escrow_refused", Message: err.Error()}
		}
		return wire.Empty{}, true, err
	case "POST /pki/escrow/drop":
		req, err := decode[wire.EscrowDropRequest](body)
		if err != nil {
			return nil, true, err
		}
		m.escrow.mu.Lock()
		defer m.escrow.mu.Unlock()
		if err := stale(req.Epoch); err != nil {
			return nil, true, err
		}
		return wire.Empty{}, true, pki.DropEscrow(dir)
	}
	return nil, false, nil
}

// escrowAfterRevocation sends the sealed copies again after the revocation list changed, so that a leader that fails now
// does not lose the revocation. A failure is only logged: the periodic round tries again.
func (m *Manager) escrowAfterRevocation(ctx context.Context) {
	if err := m.SyncEscrow(ctx); err != nil {
		m.log().Warn("sending the sealed copies of the CA key after a revocation", "error", err)
	}
}
