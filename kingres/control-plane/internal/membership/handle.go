// SPDX-License-Identifier: PostgreSQL

package membership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peers"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// Receiver is the side of a server that answers the internal calls of
// package membership. It is built on every server.
type Receiver struct {
	M *Manager
	// Authn verifies administrator credentials for a join (bootstrap/join).
	Authn auth.Authenticator
	// VerifyToken checks the setup token of this server.
	VerifyToken func(string) (bool, error)

	once    sync.Once
	limiter *auth.Limiter
}

func (r *Receiver) init() {
	r.once.Do(func() { r.limiter = auth.NewLimiter(10, time.Minute, time.Now) })
}

func bad(msg string) error {
	return &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: msg}
}

// toCallError converts a problem or an API error to the internal API's error.
func toCallError(err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return &internalapi.CallError{Status: p.Status, Code: string(p.Code), Message: p.Detail}
	}
	var ae *peerclient.APIError
	if errors.As(err, &ae) {
		return &internalapi.CallError{Status: ae.Status, Code: ae.Code, Message: ae.Message}
	}
	return err
}

func decode[T any](body []byte) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil || dec.More() {
		return v, bad("the body is not a valid request for this call")
	}
	return v, nil
}

// Handle implements internalapi.Backend.Handle.
func (r *Receiver) Handle(ctx context.Context, key, peer string, body []byte) (any, error) {
	m := r.M
	svc := m.Svc
	switch key {
	case "POST /add-server":
		req, err := decode[wire.AddServerRequest](body)
		if err != nil {
			return nil, err
		}
		res, err := m.JoinFromPeer(ctx, req)
		return res, toCallError(err)
	case "GET /instance/status":
		return svc.InstanceStatus(ctx), nil
	case "POST /instance/create":
		req, err := decode[wire.CreateInstanceRequest](body)
		if err != nil {
			return nil, err
		}
		if err := svc.CreateInstance(ctx, req); err != nil {
			return nil, err
		}
		return wire.Empty{}, nil
	case "POST /instance/start":
		return wire.Empty{}, svc.StartInstance(ctx)
	case "POST /instance/stop":
		return wire.Empty{}, svc.StopInstance(ctx)
	case "POST /instance/remove":
		return wire.Empty{}, svc.RemoveInstance(ctx)
	case "POST /instance/trust":
		req, err := decode[wire.TrustRequest](body)
		if err != nil {
			return nil, err
		}
		return wire.Empty{}, svc.ApplyTrust(ctx, req)
	case "POST /pki/install":
		req, err := decode[wire.PKIRequest](body)
		if err != nil {
			return nil, err
		}
		if err := svc.InstallPKI(ctx, req); err != nil {
			return nil, err
		}
		if req.ServerCert != "" && m.OnCertificateInstalled != nil {
			m.OnCertificateInstalled()
		}
		return wire.Empty{}, nil
	case "POST /peers":
		req, err := decode[wire.PeersRequest](body)
		if err != nil {
			return nil, err
		}
		list := make([]peers.Peer, len(req.Peers))
		for i, p := range req.Peers {
			list[i] = peers.Peer{ServerID: p.ServerID, InternalAddr: p.InternalAddress, Ordinal: p.Ordinal}
			if err := list[i].Validate(); err != nil {
				return nil, bad(err.Error())
			}
		}
		old := m.knownPeers()
		if err := m.Peers.Replace(list); err != nil {
			return nil, err
		}
		for _, p := range list {
			delete(old, p.ServerID)
		}
		for id := range old {
			m.Client.Forget(id)
		}
		return wire.Empty{}, nil
	case "POST /globals/apply":
		req, err := decode[wire.Globals](body)
		if err != nil {
			return nil, err
		}
		if err := svc.ApplyGlobals(ctx, req); err != nil {
			return nil, &internalapi.CallError{Status: http.StatusUnprocessableEntity, Code: "statement_failed", Message: err.Error()}
		}
		return wire.Empty{}, nil
	case "POST /node/finish":
		req, err := decode[wire.FinishRequest](body)
		if err != nil {
			return nil, err
		}
		return wire.Empty{}, svc.FinishMember(req.ClusterID, req.ClusterName)
	case "POST /node/reset":
		if err := m.ResetLocal(ctx); err != nil {
			return nil, err
		}
		return wire.Empty{}, nil
	case "POST /leave-cluster":
		req, err := decode[wire.LeaveRequest](body)
		if err != nil {
			return nil, err
		}
		if m.Ops == nil {
			return nil, &internalapi.CallError{Status: http.StatusNotImplemented, Code: "not_available", Message: "this server cannot run operations"}
		}
		res, err := m.LeaveFromPeer(ctx, peer, req)
		return res, toCallError(err)
	case "POST /leave-status":
		req, err := decode[wire.LeaveStatusRequest](body)
		if err != nil {
			return nil, err
		}
		if m.Ops == nil {
			return nil, &internalapi.CallError{Status: http.StatusNotImplemented, Code: "not_available", Message: "this server cannot run operations"}
		}
		res, err := m.LeaveStatus(ctx, peer, req)
		return res, toCallError(err)
	case "POST /apply":
		req, err := decode[wire.ApplyRequest](body)
		if err != nil {
			return nil, err
		}
		if m.Ops == nil {
			return nil, &internalapi.CallError{Status: http.StatusNotImplemented, Code: "not_available", Message: "this server cannot run operations"}
		}
		if err := m.Ops.ApplyLocal(ctx, req.Kind, req.Params); err != nil {
			var ce *internalapi.CallError
			if errors.As(err, &ce) {
				return nil, err
			}
			return nil, &internalapi.CallError{Status: http.StatusUnprocessableEntity, Code: "apply_failed", Message: err.Error()}
		}
		return wire.Empty{}, nil
	}
	if res, ok, err := r.handleCertCall(ctx, key, peer, body); ok {
		return res, err
	}
	return nil, &internalapi.CallError{Status: http.StatusNotFound, Code: "not_found", Message: "No such call."}
}

func (m *Manager) knownPeers() map[string]bool {
	out := map[string]bool{}
	for _, p := range m.Peers.All() {
		out[p.ServerID] = true
	}
	return out
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// Bootstrap implements internalapi.Backend.Bootstrap: the calls of the first
// contact, authenticated by the setup token of this server (prepare, install)
// or by an administrator's credentials (join).
func (r *Receiver) Bootstrap(ctx context.Context, key string, req *http.Request, body []byte) (any, error) {
	r.init()
	addr := remoteHost(req)
	if blocked, _ := r.limiter.Blocked(addr); blocked {
		return nil, &internalapi.CallError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "Too many failed attempts; try again later."}
	}
	m := r.M
	switch key {
	case "POST /bootstrap/prepare", "POST /bootstrap/install":
		ok, err := r.VerifyToken(req.Header.Get("X-Setup-Token"))
		if errors.Is(err, errTokenGone) || err != nil {
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "node_state_invalid", Message: "This server is already initialized; it cannot be added."}
		}
		if !ok {
			r.limiter.Record(addr)
			m.log().Warn("invalid setup token on the internal API", "remote", addr)
			return nil, &internalapi.CallError{Status: http.StatusUnauthorized, Code: "setup_token_invalid", Message: "The setup token is not valid."}
		}
		if key == "POST /bootstrap/prepare" {
			if m.Svc.SetupBusy(ctx) && !m.Svc.Joining() {
				return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "operation_in_progress", Message: "A setup task is running on this server."}
			}
			return prepareAnswer(m)
		}
		in, err := decode[wire.InstallRequest](body)
		if err != nil {
			return nil, err
		}
		if err := m.Svc.InstallMember([]byte(in.CACert), []byte(in.ServerCert)); err != nil {
			return nil, &internalapi.CallError{Status: http.StatusUnprocessableEntity, Code: "certificate_refused", Message: err.Error()}
		}
		return wire.Empty{}, nil
	case "POST /bootstrap/join":
		return r.join(ctx, addr, body)
	}
	return nil, &internalapi.CallError{Status: http.StatusNotFound, Code: "not_found", Message: "No such call."}
}

var errTokenGone = errors.New("token no longer valid")

func prepareAnswer(m *Manager) (any, error) {
	res, err := m.Svc.PrepareMember(m.Version)
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "node_state_invalid", Message: err.Error()}
	}
	return res, nil
}

// join verifies an administrator and hands the request to the leader. The
// credentials are used for this check only and go no further.
func (r *Receiver) join(ctx context.Context, addr string, body []byte) (any, error) {
	m := r.M
	req, err := decode[wire.JoinRequest](body)
	if err != nil {
		return nil, err
	}
	if len(req.Username) < 1 || len(req.Username) > 63 || len(req.Password) < 1 || len(req.Password) > 1024 {
		return nil, bad("the credentials are not valid")
	}
	if ok, _ := r.limiter.Allow("join-user:" + req.Username); !ok {
		return nil, &internalapi.CallError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "Too many attempts; try again later."}
	}
	roles, err := r.Authn.Authenticate(ctx, req.Username, req.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		r.limiter.Record(addr)
		return nil, &internalapi.CallError{Status: http.StatusUnauthorized, Code: "invalid_credentials", Message: "The user name or password is wrong."}
	case err != nil:
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The credentials cannot be checked now."}
	case !slices.Contains(roles, "kingres_admin"):
		return nil, &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "Only a cluster administrator (kingres_admin) can add a server."}
	}
	ar := wire.AddServerRequest{Actor: req.Username, ServerID: req.ServerID, CSR: req.CSR, PGPort: req.PGPort,
		InternalAddress: req.InternalAddress, ServerGroup: req.ServerGroup, MemoryMegabytes: req.MemoryMegabytes}
	leaderID, err := m.Leaders.Get(ctx)
	if err != nil || leaderID == "" {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "leader_unavailable", Message: "The manager leader is not known."}
	}
	if leaderID == m.Self {
		res, err := m.JoinFromPeer(ctx, ar)
		return res, toCallError(err)
	}
	var res wire.JoinResponse
	if err := m.do(ctx, leaderID, "POST", "/add-server", ar, &res); err != nil {
		return nil, toCallError(err)
	}
	return res, nil
}
