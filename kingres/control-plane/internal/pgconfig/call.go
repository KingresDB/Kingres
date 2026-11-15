// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// Calls lists the keys of the internal calls, for registration (internal-api.md, "Configuration").
var Calls = []string{CallState, CallApply, CallCheck, CallRestart, CallTime}

// HandleCall answers one internal call with the raw JSON body. The caller was authorized by the
// internal API (the leader for every call but the clock probe); the leader's calls to its own server
// come through here too.
func (a *Agent) HandleCall(ctx context.Context, key string, body []byte) (any, error) {
	bad := func(err error) error {
		return &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: err.Error()}
	}
	fail := func(err error) error {
		a.log().Warn("configuration: a call failed", "call", key, "error", err)
		return &internalapi.CallError{Status: http.StatusConflict, Code: "config_failed", Message: err.Error()}
	}
	switch key {
	case CallTime:
		return a.Time(ctx), nil
	case CallState:
		var r StateRequest
		if len(body) > 0 {
			if err := json.Unmarshal(body, &r); err != nil {
				return nil, bad(errors.New("the body is not a state request"))
			}
		}
		res, err := a.State(ctx, r)
		if err != nil {
			return nil, fail(err)
		}
		return res, nil
	case CallApply:
		var r ApplyRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, bad(errors.New("the body is not an apply request"))
		}
		res, err := a.Apply(ctx, r)
		if err != nil {
			return nil, fail(err)
		}
		return res, nil
	case CallCheck:
		var r CheckRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, bad(errors.New("the body is not a check request"))
		}
		res, err := a.Check(ctx, r)
		if err != nil {
			return nil, fail(err)
		}
		return res, nil
	case CallRestart:
		var r RestartRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, bad(errors.New("the body is not a restart request"))
		}
		if err := a.RestartInstance(ctx, r.Instance); err != nil {
			return nil, fail(err)
		}
		return struct{}{}, nil
	}
	return nil, bad(errors.New("unknown call"))
}
