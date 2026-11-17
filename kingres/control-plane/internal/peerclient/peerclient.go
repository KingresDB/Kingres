// SPDX-License-Identifier: AGPL-3.0-only

// Package peerclient is the client side of the internal API: it calls another
// server of the cluster over TLS 1.3 with this server's certificate as the
// client certificate. The peer is verified against the cluster CA and the
// name of its server id, and its certificate's serial number against the
// revocation list this server holds.
package peerclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/gather"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peers"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// File names the client reads from the pki directory.
const (
	certFile = "server.crt"
	keyFile  = "server.key"
)

// ErrUnknownPeer means the directory has no address for the server.
var ErrUnknownPeer = errors.New("peerclient: no internal address is known for this server")

// ErrUnreachable wraps a transport failure: the peer did not answer.
var ErrUnreachable = errors.New("peerclient: the server cannot be reached")

// ErrNotMember means the server answered but presents a certificate of no
// authority this server trusts: it is not (any more) a member of this cluster,
// for example a server that reset itself. It is not ErrUnreachable: waiting does
// not change it.
var ErrNotMember = errors.New("peerclient: the server does not present a certificate of this cluster")

// errBlocked marks the failure that the fault hook of the tests makes on purpose.
var errBlocked = errors.New("blocked on purpose")

// APIError is an error answer of the internal API.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("internal API answered %d %s: %s", e.Status, e.Code, e.Message)
}

// Config configures a Client.
type Config struct {
	// Self is this server's id: the sender named in the proof of a forwarded
	// request.
	Self string
	// PKIDir holds ca.crt, server.crt and server.key of this server.
	PKIDir string
	// Peers is the directory of internal addresses.
	Peers *peers.Directory
	// Revoked returns the revoked serial numbers this server knows (hex); nil
	// means none.
	Revoked func() []string
	// Timeout bounds one call (default 30 seconds).
	Timeout time.Duration
	// Fault is the fault-injection hook of the tests (nil in production).
	Fault *fault.Hook
}

// Client calls peers. It is safe for concurrent use.
type Client struct {
	cfg Config

	mu   sync.Mutex
	http map[string]*http.Client
}

// New builds a client.
func New(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Client{cfg: cfg, http: map[string]*http.Client{}}
}

func (c *Client) tlsConfig(serverID string, checkRevoked bool) (*tls.Config, error) {
	caPEM, err := os.ReadFile(filepath.Join(c.cfg.PKIDir, pki.CACertFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return nil, fmt.Errorf("peerclient: reading the CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("peerclient: the CA certificate is not valid PEM")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: serverID,
		// The certificate is read at every handshake so that a renewed
		// certificate is used without a restart.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			pair, err := tls.LoadX509KeyPair(filepath.Join(c.cfg.PKIDir, certFile), filepath.Join(c.cfg.PKIDir, keyFile))
			if err != nil {
				return nil, err
			}
			return &pair, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if !checkRevoked || c.cfg.Revoked == nil || len(cs.PeerCertificates) == 0 {
				return nil
			}
			sn := pki.SerialHex(cs.PeerCertificates[0].SerialNumber)
			for _, r := range c.cfg.Revoked() {
				if r == sn {
					return pki.ErrRevoked
				}
			}
			return nil
		},
	}, nil
}

func (c *Client) clientFor(serverID string, checkRevoked bool) (*http.Client, error) {
	key := serverID
	if !checkRevoked {
		key += "|unchecked"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if hc, ok := c.http[key]; ok {
		return hc, nil
	}
	cfg, err := c.tlsConfig(serverID, checkRevoked)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Timeout:       c.cfg.Timeout,
		Transport:     &http.Transport{TLSClientConfig: cfg, MaxIdleConnsPerHost: 2, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c.http[key] = hc
	return hc, nil
}

// Forget drops the connections to a server (after its certificate changed or
// it was removed).
func (c *Client) Forget(serverID string) {
	c.mu.Lock()
	var drop []*http.Client
	for _, k := range []string{serverID, serverID + "|unchecked"} {
		if hc := c.http[k]; hc != nil {
			drop = append(drop, hc)
		}
		delete(c.http, k)
	}
	c.mu.Unlock()
	for _, hc := range drop {
		hc.CloseIdleConnections()
	}
}

// Do calls method path (below /internal/v1) on a server, sending in as JSON
// when it is not nil and decoding the answer into out when it is not nil. It
// returns an *APIError for a non-2xx answer and wraps ErrUnreachable when the
// server did not answer.
func (c *Client) Do(ctx context.Context, serverID, method, path string, in, out any) error {
	return c.do(ctx, serverID, true, method, path, in, out)
}

// DoUnchecked is Do for a server whose certificate may be revoked: the
// revocation list is not consulted for its certificate (the chain, the name and
// the validity still are). It is for the calls that must reach a server that
// was cut off: giving it the revocation list and telling it to reset itself.
func (c *Client) DoUnchecked(ctx context.Context, serverID, method, path string, in, out any) error {
	return c.do(ctx, serverID, false, method, path, in, out)
}

// ErrNotReady is the answer of a server that is up but cannot serve yet: 503 service_unavailable,
// for example while its instance is held back until its group lease is won. It is transient.
var ErrNotReady = errors.New("peerclient: the server is not ready")

// Is makes a 503 service_unavailable answer match ErrNotReady, and the 409 "fenced" answer of a
// callee that saw a newer run of the task match fence.ErrFenced (the task engine then stands
// down instead of failing the task).
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotReady:
		return e.Status == http.StatusServiceUnavailable && e.Code == "service_unavailable"
	case fence.ErrFenced:
		return e.Status == http.StatusConflict && e.Code == "fenced"
	}
	return false
}

// NotReadyBound is how long a task step keeps retrying a server that answers "not ready" before
// the step fails (a variable so that tests can shorten it).
var NotReadyBound = 5 * time.Minute

// transportTries is how often a call that is safe to repeat is made when the connection fails under
// it (one transient failure must not end a task step).
const transportTries = 3

// repeatable says whether a call may be made again after the connection failed under it, when it is
// not known whether the server received the first one. A reading (GET) always may. Of the calls that
// change something, the ones a task step makes may too: the engine runs a step again from its start after
// any interruption (a restart of kingresd, a lease that was taken over), so every call of a step has to
// give the same result when it is repeated, and a repeat that arrives after a newer run of the task has
// started is refused by the callee through the fence. A call outside a task (a forwarded request of a
// person, a certificate renewal, a lease request that the rounds repeat anyway) is not repeated here.
func repeatable(inStep bool, method string) bool { return inStep || method == http.MethodGet }

// transient says whether err is a failure of the connection that another try may not meet: not a
// refusal of the callee, not a server that is not a member, not a certificate that is revoked, not a
// partition that a test made on purpose, not the end of the caller's own time.
func transient(ctx context.Context, err error) bool {
	return errors.Is(err, ErrUnreachable) && ctx.Err() == nil && !errors.Is(err, errBlocked) && !errors.Is(err, pki.ErrRevoked) && !clientTimedOut(err)
}

// clientTimedOut says that the client's own time limit ended while it waited for the answer of a connection that
// was made (ST-03): the callee may be working or hung, and the same call at the full limit again is the wrong answer to
// it. A timeout while the connection was made (dial) is a failure that no request has seen, and is repeated.
func clientTimedOut(err error) bool {
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		return false
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return false
	}
	return true
}

func (c *Client) do(ctx context.Context, serverID string, checkRevoked bool, method, path string, in, out any) error {
	_, inStep := fence.From(ctx)
	start := time.Now()
	tries := 0
	for delay := time.Second; ; {
		err := c.doOnce(ctx, serverID, checkRevoked, method, path, in, out)
		if err != nil && repeatable(inStep, method) && transient(ctx, err) && tries+1 < transportTries {
			tries++
			// The connection may be spoiled: do not reuse it.
			c.Forget(serverID)
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Duration(tries) * 400 * time.Millisecond):
			}
			continue
		}
		// A task step (its context carries the fence) waits for a server that is not ready; a
		// request of a user does not (it gets the 503 and its Retry-After).
		if err == nil || !inStep || !errors.Is(err, ErrNotReady) {
			return err
		}
		if time.Since(start) > NotReadyBound {
			return fmt.Errorf("server %s was still waiting for its group lease after %s: %w", serverID, NotReadyBound, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 8*time.Second {
			delay *= 2
		}
	}
}

func (c *Client) doOnce(ctx context.Context, serverID string, checkRevoked bool, method, path string, in, out any) error {
	if c.cfg.Fault.OutboundBlocked(serverID) {
		return fmt.Errorf("%w: %w: %s: blocked by the fault hook", ErrUnreachable, errBlocked, serverID)
	}
	p, ok := c.cfg.Peers.Get(serverID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPeer, serverID)
	}
	hc, err := c.clientFor(serverID, checkRevoked)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	target, err := internalapi.URL(p.InternalAddr, path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set(internalapi.HeaderVersion, internalapi.Version)
	// A step of a task run names its run, so that the callee can refuse a run that was superseded.
	if tok, ok := fence.From(ctx); ok {
		req.Header.Set(fence.Header, tok.String())
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &unknown) {
			return fmt.Errorf("%w: %s", ErrNotMember, serverID)
		}
		return fmt.Errorf("%w: %s: %w", ErrUnreachable, serverID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnreachable, serverID, err)
	}
	if resp.StatusCode/100 != 2 {
		var pe struct{ Error, Message string }
		_ = json.Unmarshal(data, &pe)
		return &APIError{Status: resp.StatusCode, Code: peerText(pe.Error, 64), Message: peerText(pe.Message, MaxPeerText)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("peerclient: the answer of %s is not valid: %w", serverID, err)
		}
	}
	return nil
}

// prove adds the proof of the session of req.Identity to req (nothing when
// the session has no key: the receiver then refuses the call).
func (c *Client) prove(kind fwdproof.Kind, to string, req *internalapi.ForwardRequest) error {
	if len(req.Identity.ProofKey) == 0 {
		return nil
	}
	p, err := fwdproof.Sign(req.Identity.ProofKey, req.ProofMessage(kind, c.cfg.Self, to), time.Now())
	if err != nil {
		return err
	}
	req.Proof = &p
	return nil
}

// AuthorizeGather sends the leader the proof of the user's session for a gather
// call and returns the leader's assertion, which the members that answer the call
// accept instead of a proof. The proof reveals the user's ClientKey to its
// receiver, so it goes to the leader only (package fwdproof).
func (c *Client) AuthorizeGather(ctx context.Context, leaderID string, req internalapi.ForwardRequest) (fwdproof.Assertion, error) {
	var a fwdproof.Assertion
	if len(req.Identity.ProofKey) == 0 {
		return a, errors.New("peerclient: the session has no key to prove itself with")
	}
	req.Assertion = nil
	if err := c.prove(fwdproof.KindGatherAuthorize, leaderID, &req); err != nil {
		return a, err
	}
	err := c.Do(ctx, leaderID, http.MethodPost, "/gather-authorize", req, &a)
	return a, err
}

// Forward sends a forwarded operation to the leader.
func (c *Client) Forward(ctx context.Context, leaderID string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
	var resp internalapi.ForwardResponse
	if err := c.prove(fwdproof.KindForward, leaderID, &req); err != nil {
		return resp, err
	}
	err := c.Do(ctx, leaderID, http.MethodPost, "/forward", req, &resp)
	return resp, err
}

// Forwarder adapts a Client and a leader resolver to the HTTP layer's
// Forwarder interface.
type Forwarder struct {
	// Self is this server's id.
	Self string
	// Leaders reads the manager leader from the catalog.
	Leaders *leader.Resolver
	// Client calls the leader.
	Client *Client
}

// Leader returns the leader's id and whether it is this server.
func (f *Forwarder) Leader(ctx context.Context) (string, bool, error) {
	id, err := f.Leaders.Get(ctx)
	if err != nil {
		return "", false, err
	}
	return id, id != "" && id == f.Self, nil
}

// Forward sends the operation to the leader.
func (f *Forwarder) Forward(ctx context.Context, leaderID string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
	return f.Client.Forward(ctx, leaderID, req)
}

// ForgetAll drops the connections to every server. It is called after this
// server's certificate was replaced: a connection that was opened with the old
// certificate is checked against its end of validity by the other server at
// every request, so it must not be used after the old certificate expires.
func (c *Client) ForgetAll() {
	c.mu.Lock()
	all := c.http
	c.http = map[string]*http.Client{}
	c.mu.Unlock()
	for _, hc := range all {
		hc.CloseIdleConnections()
	}
}

// gatherBody is the body of a gather call: the request with the assertion of the
// leader and never a proof, which would reveal the user's ClientKey to the member
// that receives it.
func gatherBody(req internalapi.ForwardRequest) ([]byte, error) {
	req.Proof = nil
	return json.Marshal(req)
}

// Gather asks a server for its part of a cluster-wide read (POST /gather) and
// returns its answer: the status and body of the operation it ran. The answer
// is read through a limit of maxBytes (a larger one is gather.ErrInvalid), an
// error status of the internal API itself is gather.ErrRefused and a transport
// failure is gather.ErrUnreachable, or context.DeadlineExceeded when the
// caller's deadline passed. The status of the operation is for the caller to judge.
func (c *Client) Gather(ctx context.Context, serverID string, req internalapi.ForwardRequest, maxBytes int64) (internalapi.ForwardResponse, error) {
	if c.cfg.Fault.OutboundBlocked(serverID) {
		return internalapi.ForwardResponse{}, fmt.Errorf("%w: blocked by the fault hook", gather.ErrUnreachable)
	}
	p, ok := c.cfg.Peers.Get(serverID)
	if !ok {
		return internalapi.ForwardResponse{}, fmt.Errorf("%w: %s", gather.ErrUnreachable, ErrUnknownPeer)
	}
	hc, err := c.clientFor(serverID, true)
	if err != nil {
		return internalapi.ForwardResponse{}, fmt.Errorf("%w: %w", gather.ErrUnreachable, err)
	}
	b, err := gatherBody(req)
	if err != nil {
		return internalapi.ForwardResponse{}, err
	}
	target, err := internalapi.URL(p.InternalAddr, "/gather")
	if err != nil {
		return internalapi.ForwardResponse{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return internalapi.ForwardResponse{}, err
	}
	hreq.Header.Set(internalapi.HeaderVersion, internalapi.Version)
	hreq.Header.Set("Content-Type", "application/json")
	// The deadline of the caller bounds the call; the client's own timeout is the larger one.
	resp, err := hc.Do(hreq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return internalapi.ForwardResponse{}, context.DeadlineExceeded
		}
		return internalapi.ForwardResponse{}, fmt.Errorf("%w: %w", gather.ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return internalapi.ForwardResponse{}, context.DeadlineExceeded
		}
		return internalapi.ForwardResponse{}, fmt.Errorf("%w: %w", gather.ErrUnreachable, err)
	}
	if int64(len(data)) > maxBytes {
		return internalapi.ForwardResponse{}, gather.ErrInvalid
	}
	if resp.StatusCode/100 != 2 {
		return internalapi.ForwardResponse{}, gather.ErrRefused
	}
	var fr internalapi.ForwardResponse
	if err := json.Unmarshal(data, &fr); err != nil {
		return internalapi.ForwardResponse{}, gather.ErrInvalid
	}
	return fr, nil
}
