// SPDX-License-Identifier: PostgreSQL

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
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	// PKIDir holds ca.crt, server.crt and server.key of this server.
	PKIDir string
	// Peers is the directory of internal addresses.
	Peers *peers.Directory
	// Revoked returns the revoked serial numbers this server knows (hex); nil
	// means none.
	Revoked func() []string
	// Timeout bounds one call (default 30 seconds).
	Timeout time.Duration
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

func (c *Client) tlsConfig(serverID string) (*tls.Config, error) {
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
			if c.cfg.Revoked == nil || len(cs.PeerCertificates) == 0 {
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

func (c *Client) clientFor(serverID string) (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hc, ok := c.http[serverID]; ok {
		return hc, nil
	}
	cfg, err := c.tlsConfig(serverID)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Timeout:       c.cfg.Timeout,
		Transport:     &http.Transport{TLSClientConfig: cfg, MaxIdleConnsPerHost: 2, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c.http[serverID] = hc
	return hc, nil
}

// Forget drops the connections to a server (after its certificate changed or
// it was removed).
func (c *Client) Forget(serverID string) {
	c.mu.Lock()
	hc := c.http[serverID]
	delete(c.http, serverID)
	c.mu.Unlock()
	if hc != nil {
		hc.CloseIdleConnections()
	}
}

// Do calls method path (below /internal/v1) on a server, sending in as JSON
// when it is not nil and decoding the answer into out when it is not nil. It
// returns an *APIError for a non-2xx answer and wraps ErrUnreachable when the
// server did not answer.
func (c *Client) Do(ctx context.Context, serverID, method, path string, in, out any) error {
	p, ok := c.cfg.Peers.Get(serverID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPeer, serverID)
	}
	hc, err := c.clientFor(serverID)
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
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
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

// Forward sends a forwarded operation to the leader.
func (c *Client) Forward(ctx context.Context, leaderID string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
	var resp internalapi.ForwardResponse
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
