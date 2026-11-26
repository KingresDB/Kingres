// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// CallRepoint is the internal call that points a standby at a new primary (design
// ha-phase5-part2.md, 3.3 F8).
const CallRepoint = "POST /standby/repoint"

// RepointRequest is the body of POST /standby/repoint.
type RepointRequest struct {
	Group        int32  `json:"group"`
	StandbyID    int64  `json:"standbyId"`
	InstanceID   int    `json:"instanceId"`
	PrimaryHost  string `json:"primaryHost"`
	PrimaryPort  int    `json:"primaryPort"`
	ExpectedTerm int64  `json:"expectedTerm"`
}

// RepointAnswer is the answer of POST /standby/repoint.
type RepointAnswer struct {
	Streaming bool   `json:"streaming"`
	State     string `json:"state"`
}

// EmptySyncNames takes synchronous_standby_names out of the managed settings of the
// instance in directory dir and reloads it. A promoted standby inherits the list of its
// old primary, and every commit on it would then wait for a standby that is not there
// (extension, precondition 3). It is harmless while the instance is in recovery.
func (a *Agent) EmptySyncNames(ctx context.Context, dir int) error {
	settings, err := a.mgr().Settings(dir)
	if err != nil {
		return fmt.Errorf("standby: reading the managed settings: %w", err)
	}
	if _, has := settings.Extra[syncNamesKey]; !has {
		return nil
	}
	next := settings
	next.Extra = maps.Clone(settings.Extra)
	delete(next.Extra, syncNamesKey)
	if err := a.mgr().WriteManaged(dir, next); err != nil {
		return fmt.Errorf("standby: writing the managed settings: %w", err)
	}
	if up, err := a.mgr().Running(ctx, dir); err == nil && up {
		if err := a.mgr().Reload(ctx, dir); err != nil {
			return fmt.Errorf("standby: reloading the instance: %w", err)
		}
	}
	return nil
}

// Repoint makes the standby in directory InstanceID follow the new primary: the slot of
// the standby is created on it, primary_conninfo and primary_slot_name are rewritten and
// the instance reloaded, and the walreceiver is waited for (bound wait) until it
// streams. A standby that does not stream by then is ahead of the new primary or on
// another timeline; the answer says so and the caller marks it for a rebuild.
func (a *Agent) Repoint(ctx context.Context, r RepointRequest, wait time.Duration) (RepointAnswer, error) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	dir := r.InstanceID
	if dir == 0 {
		dir = instance.StandbyDirID(r.Group)
	}
	if err := a.provedStandby(ctx, dir, false); err != nil {
		return RepointAnswer{}, err
	}
	e, err := a.metaOf(dir)
	if err != nil {
		return RepointAnswer{}, err
	}
	if e.ID != r.StandbyID {
		return RepointAnswer{}, ErrConflict
	}
	slot := SlotName(e.ID)
	moved := e
	moved.PrimaryServer, moved.PrimaryPort = r.PrimaryHost, r.PrimaryPort
	if err := a.ensureSlot(ctx, moved, slot); err != nil {
		return RepointAnswer{}, err
	}
	conninfo, err := a.Svc.ReplicationConninfo(r.PrimaryHost, r.PrimaryPort, AppName(e.ID), "")
	if err != nil {
		return RepointAnswer{}, fmt.Errorf("standby: building the replication connection: %w", err)
	}
	settings, err := a.mgr().Settings(dir)
	if err != nil {
		return RepointAnswer{}, fmt.Errorf("standby: reading the managed settings: %w", err)
	}
	if settings.Extra["primary_conninfo"] != conninfo || settings.Extra["primary_slot_name"] != slot {
		next := settings
		next.Extra = maps.Clone(settings.Extra)
		if next.Extra == nil {
			next.Extra = map[string]string{}
		}
		next.Extra["primary_conninfo"] = conninfo
		next.Extra["primary_slot_name"] = slot
		if err := a.mgr().WriteManaged(dir, next); err != nil {
			return RepointAnswer{}, fmt.Errorf("standby: writing the managed settings: %w", err)
		}
		if err := a.writeMeta(dir, moved); err != nil {
			return RepointAnswer{}, fmt.Errorf("standby: recording the standby: %w", err)
		}
		if err := a.mgr().Reload(ctx, dir); err != nil {
			return RepointAnswer{}, fmt.Errorf("standby: reloading the standby: %w", err)
		}
	}
	deadline := a.Clock.Mono() + wait
	for {
		ls := LocalStatus{Port: moved.Port}
		if mk, err := a.mgr().Managed(dir); err == nil {
			ls.Port = mk.Port
		}
		a.readReceiver(ctx, dir, &ls)
		if ls.Receiving == "streaming" {
			return RepointAnswer{Streaming: true, State: ls.Receiving}, nil
		}
		if a.Clock.Mono() >= deadline {
			return RepointAnswer{Streaming: false, State: ls.Receiving}, nil
		}
		select {
		case <-ctx.Done():
			return RepointAnswer{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// handleRepoint is the call POST /standby/repoint.
func (a *Agent) handleRepoint(ctx context.Context, body []byte) (any, error) {
	var r RepointRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &internalapi.CallError{Status: 400, Code: "bad_request", Message: "The body is not a repoint request."}
	}
	if r.PrimaryHost == "" || r.PrimaryPort <= 0 {
		return nil, &internalapi.CallError{Status: 400, Code: "bad_request", Message: "A primary host and port are required."}
	}
	ans, err := a.Repoint(ctx, r, 60*time.Second)
	if errors.Is(err, ErrNotProvedStandby) || errors.Is(err, ErrConflict) {
		return nil, &internalapi.CallError{Status: 409, Code: "not_standby", Message: "The instance is not the standby the request names."}
	}
	return ans, err
}
