// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerstate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// The internal calls of the record of what was published (internal-api.md, "The record of what was published"). Both are LeaderOnly: the leader
// gives a holder its state to merge, and a server that is about to lead and has none asks the others for theirs.
const (
	CallGet   = "POST /ledger/state/get"
	CallMerge = "POST /ledger/state/merge"
)

// Calls lists the keys of the internal calls the agent answers.
var Calls = []string{CallGet, CallMerge}

// Agent answers the calls on a server.
type Agent struct{ File *File }

func bad(code, msg string, status int) error {
	return &internalapi.CallError{Status: status, Code: code, Message: msg}
}

// Union is the monotone union of two states: the larger size per log and the larger event count; two that disagree on one size or on the
// last event are ErrFork.
func Union(a, b State) (State, error) {
	next := clone(a)
	for k, v := range b.Logs {
		m, ok := next.Logs[k]
		switch {
		case !ok || v.Size > m.Size:
			next.Logs[k] = v
		case v.Size == m.Size && v.Root != m.Root:
			return State{}, ErrFork
		}
	}
	switch {
	case b.Events > next.Events:
		next.Events, next.EventHash = b.Events, b.EventHash
	case b.Events == next.Events && b.EventHash != next.EventHash:
		return State{}, ErrFork
	}
	return next, nil
}

// Handle runs one call; the body is raw JSON.
func (a *Agent) Handle(_ context.Context, key string, body []byte) (any, error) {
	switch key {
	case CallGet:
		if !a.File.Initialized() {
			return nil, bad("no_state", "This server has no record of what was published.", http.StatusNotFound)
		}
		return a.File.Snapshot(), nil
	case CallMerge:
		var s State
		if err := json.Unmarshal(body, &s); err != nil || len(body) > MaxFile {
			return nil, bad("bad_request", "The body is not valid.", http.StatusBadRequest)
		}
		if err := a.File.Merge(s); err != nil {
			if errors.Is(err, ErrFork) {
				return nil, bad("fork", "The state contradicts the record of this server.", http.StatusConflict)
			}
			return nil, bad("unavailable", "The record cannot be changed.", http.StatusServiceUnavailable)
		}
		return struct{}{}, nil
	}
	return nil, bad("not_found", "No such call.", http.StatusNotFound)
}
