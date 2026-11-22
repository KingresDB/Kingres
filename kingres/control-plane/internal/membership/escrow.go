// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// The escrow of the key of the certificate authority (ha-phase5-part2.md, section 3.4): the leader seals the CA key and
// the revocation list to each server that may become leader and sends each its copy; a new leader opens its copy and
// goes on (pki.AdoptEscrow). The cryptography is package pki; this file is the leader's side: who gets a copy, when it is
// sent again, and the operations of the operator.

// EscrowRefresh is how old a copy may get before the leader seals and sends it again (it also repairs a server that lost
// its file). A copy is sent at once when the revocation list or the set of recipients changes.
const EscrowRefresh = 6 * time.Hour

const escrowCallTimeout = 20 * time.Second

// escrowGuard serializes the escrow files of this server (the bookkeeping, the keys and the copy).
type escrowGuard struct {
	mu    sync.Mutex
	recMu sync.Mutex // guards escrowRecipients only (read while mu is held)
}

// EscrowRecipients returns the ids of the servers that may become leader, from the standbys of the map group. Nil means
// every active member other than this server.
type EscrowRecipients func(ctx context.Context) ([]string, error)

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

// escrowCall is how the leader reaches a server for the escrow: the peer client (a revoked or removed server is reached
// unchecked). Tests replace it with the receivers of other servers.
func (m *Manager) escrowCall(ctx context.Context, id, path string, in, out any, unchecked bool) error {
	if m.escrowTransport != nil {
		return m.escrowTransport(ctx, id, path, in, out)
	}
	if unchecked {
		return m.doUnchecked(ctx, id, "POST", path, in, out)
	}
	return m.do(ctx, id, "POST", path, in, out)
}

// SetEscrowRecipients sets where the recipients come from (kingresd sets it to the servers that host a ready standby of
// the map group).
func (m *Manager) SetEscrowRecipients(f EscrowRecipients) {
	m.escrow.recMu.Lock()
	defer m.escrow.recMu.Unlock()
	m.escrowRecipients = f
}

func (m *Manager) escrowCandidates(ctx context.Context) ([]string, error) {
	active, err := m.escrowActiveServers(ctx)
	if err != nil {
		return nil, err
	}
	m.escrow.recMu.Lock()
	f := m.escrowRecipients
	m.escrow.recMu.Unlock()
	if f == nil {
		return active, nil
	}
	want, err := f(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range want {
		// A server that is not an active member (removed, leaving, revoked) never gets a copy.
		if slices.Contains(active, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

// AdoptEscrow is called on a server that has become the manager leader: when it holds no CA key it makes one from its
// sealed copy. It returns whether the server holds the CA key afterwards.
func (m *Manager) AdoptEscrow() (bool, error) {
	m.escrow.mu.Lock()
	defer m.escrow.mu.Unlock()
	res, err := pki.AdoptEscrow(m.Svc.PKIDir(), m.Self, time.Now())
	switch {
	case err != nil:
		m.log().Error("the sealed copy of the CA key could not be adopted", "error", err)
		return false, err
	case res == pki.Adopted:
		m.log().Info("this server adopted the CA key from its sealed copy; certificates can be issued and revoked here")
		return true, nil
	case res == pki.AdoptHeld:
		return true, nil
	}
	return false, nil
}

func (m *Manager) escrowStatusLocked(ctx context.Context, st pki.EscrowState, ca *pki.CA) api.CaEscrowStatus {
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
		if rs.LastError != "" {
			e := rs.LastError
			r.Error = &e
			if rs.LastRefused {
				r.State = "refused"
			}
		} else if rs.KeyID != "" && rs.Generation == st.Generation && (hash == "" || rs.ContentHash == hash) {
			r.State = "current"
		}
		if r.State != "current" {
			complete = false
		}
		out.Recipients = append(out.Recipients, r)
	}
	out.Complete = complete
	switch {
	case !st.Enabled:
		out.Message = "Escrow is off: a new manager leader has no CA key until an operator restores it by hand."
	case ca == nil:
		out.Message = "This server holds no CA key; ask the manager leader."
	case len(st.Recipients) == 0:
		out.Message = "No server hosts a ready standby of the map group yet, so no copy of the CA key exists outside the leader."
	case !complete:
		out.Message = "Some recipients do not hold the newest copy yet; the leader sends it again every minute."
	default:
		out.Message = "Every server that can become leader holds a sealed copy of the CA key."
	}
	return out
}

// EscrowStatus describes the escrow as the leader sees it.
func (m *Manager) EscrowStatus(ctx context.Context) (api.CaEscrowStatus, error) {
	m.escrow.mu.Lock()
	defer m.escrow.mu.Unlock()
	return m.escrowStatusNow(ctx)
}

func (m *Manager) escrowStatusNow(ctx context.Context) (api.CaEscrowStatus, error) {
	st, err := pki.LoadEscrowState(m.Svc.PKIDir())
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	ca, err := m.Svc.CA()
	if err != nil {
		ca = nil
	}
	return m.escrowStatusLocked(ctx, st, ca), nil
}

// SetEscrow switches the escrow on or off (setCaEscrow) and, when it is on, sends the copies now.
func (m *Manager) SetEscrow(ctx context.Context, enabled bool) (api.CaEscrowStatus, error) {
	m.escrow.mu.Lock()
	defer m.escrow.mu.Unlock()
	ca, err := m.ca()
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	dir := m.Svc.PKIDir()
	st, err := pki.LoadEscrowState(dir)
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	if enabled {
		if !st.Enabled {
			st.Enabled = true
			st.Generation++
			if err := pki.SaveEscrowState(dir, st); err != nil {
				return api.CaEscrowStatus{}, err
			}
		}
		if err := m.syncEscrowLocked(ctx, ca, false); err != nil {
			return api.CaEscrowStatus{}, err
		}
	} else {
		st.Enabled = false
		if err := pki.SaveEscrowState(dir, st); err != nil {
			return api.CaEscrowStatus{}, err
		}
		if err := m.syncEscrowLocked(ctx, ca, false); err != nil {
			return api.CaEscrowStatus{}, err
		}
	}
	return m.escrowStatusNow(ctx)
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
	dir := m.Svc.PKIDir()
	st, err := pki.LoadEscrowState(dir)
	if err != nil {
		return api.CaEscrowStatus{}, err
	}
	if !st.Enabled {
		return api.CaEscrowStatus{}, problem.New(problem.Conflict, "The escrow of the CA key is off; switch it on with setCaEscrow first.")
	}
	st.Generation++
	if err := pki.SaveEscrowState(dir, st); err != nil {
		return api.CaEscrowStatus{}, err
	}
	if err := m.syncEscrowLocked(ctx, ca, true); err != nil {
		return api.CaEscrowStatus{}, err
	}
	return m.escrowStatusNow(ctx)
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

// syncEscrowLocked reconciles the recipients with the standbys. With rotate every recipient makes a new escrow key.
func (m *Manager) syncEscrowLocked(ctx context.Context, ca *pki.CA, rotate bool) error {
	dir := m.Svc.PKIDir()
	st, err := pki.LoadEscrowState(dir)
	if err != nil {
		return err
	}
	if !st.Enabled {
		// Switched off: every copy is taken away, and the servers that could not be reached are asked again next round.
		for _, id := range st.SortedRecipients() {
			if m.dropEscrowAt(ctx, id) == nil {
				delete(st.Recipients, id)
			}
		}
		return pki.SaveEscrowState(dir, st)
	}
	cands, err := m.escrowCandidates(ctx)
	if err != nil {
		return fmt.Errorf("working out which servers may become leader: %w", err)
	}
	for _, id := range st.SortedRecipients() {
		if !slices.Contains(cands, id) {
			// Best effort: the server may be gone. A server that cannot be told keeps a copy it cannot use
			// without the standby role, but still holds (see the threat model).
			_ = m.dropEscrowAt(ctx, id)
			delete(st.Recipients, id)
		}
	}
	hash, err := ca.EscrowContentHash()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, id := range cands {
		rs := st.Recipients[id]
		fresh := rs.KeyID != "" && rs.LastError == "" && rs.Generation == st.Generation && rs.ContentHash == hash && now.Sub(rs.SealedAt) < EscrowRefresh
		if fresh && !rotate {
			continue
		}
		rs.LastTry = now
		if err := m.sendEscrow(ctx, ca, id, st.Generation, rotate, hash, &rs); err != nil {
			rs.LastError = err.Error()
			var ae *peerclient.APIError
			rs.LastRefused = errors.As(err, &ae)
			m.log().Warn("sealed copy of the CA key not delivered", "server", id, "error", err)
		}
		st.Recipients[id] = rs
	}
	return pki.SaveEscrowState(dir, st)
}

func (m *Manager) sendEscrow(ctx context.Context, ca *pki.CA, id string, generation int64, rotate bool, hash string, rs *pki.EscrowRecipientState) error {
	octx, cancel := context.WithTimeout(ctx, escrowCallTimeout)
	defer cancel()
	var offer wire.EscrowOfferResponse
	if err := m.escrowCall(octx, id, "/pki/escrow/offer", wire.EscrowOfferRequest{Rotate: rotate}, &offer, false); err != nil {
		return fmt.Errorf("asking for the escrow key: %w", err)
	}
	pub, err := pki.DecodeKey(offer.PublicKey)
	if err != nil {
		return fmt.Errorf("the escrow key of %s is not valid", id)
	}
	now := time.Now()
	blob, err := pki.SealCA(ca, id, offer.KeyID, pub, generation, now)
	if err != nil {
		return err
	}
	if err := m.escrowCall(octx, id, "/pki/escrow/store", wire.EscrowStoreRequest{Blob: blob}, nil, false); err != nil {
		return fmt.Errorf("storing the copy: %w", err)
	}
	*rs = pki.EscrowRecipientState{KeyID: blob.KeyID, Generation: generation, SealedAt: blob.SealedAt, ContentHash: hash, LastTry: now}
	return nil
}

// dropEscrowAt tells a server to delete its copy and escrow keys. A revoked server is told as well.
func (m *Manager) dropEscrowAt(ctx context.Context, id string) error {
	octx, cancel := context.WithTimeout(ctx, escrowCallTimeout)
	defer cancel()
	return m.escrowCall(octx, id, "/pki/escrow/drop", wire.Empty{}, nil, true)
}

// ----------------------------------------------- receiving side (any server)

// handleEscrowCall answers the escrow calls of the internal API. Only the manager leader may make them (the table of
// internalapi.Calls); what is stored is checked again here.
func (r *Receiver) handleEscrowCall(_ context.Context, key string, body []byte) (any, bool, error) {
	m := r.M
	dir := m.Svc.PKIDir()
	switch key {
	case "POST /pki/escrow/offer":
		req, err := decode[wire.EscrowOfferRequest](body)
		if err != nil {
			return nil, true, err
		}
		m.escrow.mu.Lock()
		id, pub, err := pki.RecipientOffer(dir, req.Rotate, time.Now())
		m.escrow.mu.Unlock()
		if err != nil {
			return nil, true, err
		}
		return wire.EscrowOfferResponse{KeyID: id, PublicKey: pki.EncodeKey(pub)}, true, nil
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
		m.escrow.mu.Lock()
		err := pki.DropEscrow(dir)
		m.escrow.mu.Unlock()
		return wire.Empty{}, true, err
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
