// SPDX-License-Identifier: AGPL-3.0-only

package peerclient

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/relay"
)

// streamClient is the client of the streaming calls: the same mutual TLS as every other call, and no time limit of its own. The calls of
// the ordinary client end after Config.Timeout (30 s by default), which counts the whole exchange, body included; a chunk of 64 MiB or a
// download of many gigabytes takes longer. The limits of a stream are the context of the caller and the idle deadlines of the two ends.
func (c *Client) streamClient(serverID string) (*http.Client, error) {
	key := serverID + "|stream"
	c.mu.Lock()
	defer c.mu.Unlock()
	if hc, ok := c.http[key]; ok {
		return hc, nil
	}
	cfg, err := c.tlsConfig(serverID, true)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cfg, MaxIdleConnsPerHost: 2, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
			// A body that the caller reads from a slow client must not be buffered here, and the response is read as it comes.
			WriteBufferSize: 64 << 10, ReadBufferSize: 64 << 10},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c.http[key] = hc
	return hc, nil
}

// ForwardStream sends a streaming operation to the leader: one POST /forward-stream whose body is the frame of req and then body (nil for a
// download), and whose answer is the frame of the status and the headers and then the raw bytes, which the caller reads from the returned
// reader and closes. Nothing is repeated here: a body that was read cannot be read again, and the console resumes an upload at the offset
// the server holds.
func (c *Client) ForwardStream(ctx context.Context, leaderID string, req internalapi.ForwardRequest, body io.Reader) (relay.Response, io.ReadCloser, error) {
	if err := c.prove(fwdproof.KindForwardStream, leaderID, &req); err != nil {
		return relay.Response{}, nil, err
	}
	if c.cfg.Fault.OutboundBlocked(leaderID) {
		return relay.Response{}, nil, fmt.Errorf("%w: %w: %s: blocked by the fault hook", ErrUnreachable, errBlocked, leaderID)
	}
	p, ok := c.cfg.Peers.Get(leaderID)
	if !ok {
		return relay.Response{}, nil, fmt.Errorf("%w: %s", ErrUnknownPeer, leaderID)
	}
	hc, err := c.streamClient(leaderID)
	if err != nil {
		return relay.Response{}, nil, err
	}
	target, err := internalapi.URL(p.InternalAddr, "/forward-stream")
	if err != nil {
		return relay.Response{}, nil, err
	}
	dial := func(ctx context.Context, rd io.Reader) (*http.Response, error) {
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, target, rd)
		if err != nil {
			return nil, err
		}
		hr.Header.Set(internalapi.HeaderVersion, internalapi.Version)
		hr.Header.Set("Content-Type", "application/octet-stream")
		resp, err := hc.Do(hr)
		if err != nil {
			var unknown x509.UnknownAuthorityError
			if errors.As(err, &unknown) {
				return nil, fmt.Errorf("%w: %s", ErrNotMember, leaderID)
			}
			return nil, fmt.Errorf("%w: %s: %w", ErrUnreachable, leaderID, err)
		}
		return resp, nil
	}
	resp, rest, err := relay.Call(ctx, dial, req, body)
	var ce *relay.CallError
	if errors.As(err, &ce) {
		// The call itself was refused: a small JSON problem of the internal API. The same codes as for /forward.
		return relay.Response{}, nil, refusal(ce)
	}
	return resp, rest, err
}

// refusal turns the leader's refusal of the stream call into the APIError the other calls return.
func refusal(ce *relay.CallError) error {
	var pe struct{ Error, Message string }
	_ = json.Unmarshal([]byte(ce.Text), &pe)
	return &APIError{Status: ce.Status, Code: peerText(pe.Error, 64), Message: peerText(pe.Message, MaxPeerText)}
}

// ForwardStream sends the streaming operation to the leader (the Forwarder of the HTTP layer).
func (f *Forwarder) ForwardStream(ctx context.Context, leaderID string, req internalapi.ForwardRequest, body io.Reader) (relay.Response, io.ReadCloser, error) {
	return f.Client.ForwardStream(ctx, leaderID, req, body)
}
