// SPDX-License-Identifier: AGPL-3.0-only

package peerclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// ST-03: a call that timed out on the client's side after the connection was made is not repeated at the full
// timeout (the callee may be hung or may have acted); a refusal to connect and a timeout of the dial are.
func TestAClientTimeoutIsNotRepeatedButAFailedConnectionIs(t *testing.T) {
	ctx := context.Background()
	wrap := func(inner error) error { return fmt.Errorf("%w: s1: %w", ErrUnreachable, inner) }
	timedOut := wrap(&url.Error{Op: "Post", URL: "https://s1", Err: timeoutErr{}})
	if transient(ctx, timedOut) || !clientTimedOut(timedOut) {
		t.Error("a timeout of the request must not be repeated")
	}
	dial := wrap(&net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}})
	if !transient(ctx, dial) {
		t.Error("a timeout of the dial reached no server: it is repeated")
	}
	refused := wrap(errors.New("connection refused"))
	if !transient(ctx, refused) {
		t.Error("a refused connection is repeated")
	}
	reset := wrap(&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")})
	if !transient(ctx, reset) {
		t.Error("a connection that broke under the call is repeated")
	}
}
