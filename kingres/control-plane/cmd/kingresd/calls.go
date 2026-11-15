// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"net/http"
	"sync"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// callFunc answers one internal call of a feature (heartbeats, leases,
// standbys): peer is the verified server id of the caller, body the raw JSON.
type callFunc func(ctx context.Context, peer string, body []byte) (any, error)

// callTable is where the features of high availability register the internal
// calls they answer. The authorization of each call is the policy in
// internalapi.Calls; this table only dispatches.
type callTable struct {
	mu sync.RWMutex
	m  map[string]callFunc
}

func newCallTable() *callTable { return &callTable{m: map[string]callFunc{}} }

// Register adds the handler of a key such as "POST /heartbeat".
func (t *callTable) Register(key string, f callFunc) {
	t.mu.Lock()
	t.m[key] = f
	t.mu.Unlock()
}

// handle runs the handler of key; ok is false when none is registered.
func (t *callTable) handle(ctx context.Context, key, peer string, body []byte) (res any, ok bool, err error) {
	t.mu.RLock()
	f := t.m[key]
	t.mu.RUnlock()
	if f == nil {
		return nil, false, nil
	}
	res, err = f(ctx, peer, body)
	return res, true, err
}

// notStarted is the answer of a registered call whose feature is not running.
var notStarted = &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "not_available", Message: "This feature is not running on this server."}
