// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// Keys of the internal calls this package answers (see internal-api.md).
const (
	CallPlan    = "POST /standby/plan"
	CallCreate  = "POST /standby/create"
	CallRemove  = "POST /standby/remove"
	CallRebuild = "POST /standby/rebuild"
	CallStatus  = "GET /standby/status"
)

// Calls lists the keys, for registration.
var Calls = []string{CallPlan, CallCreate, CallRemove, CallRebuild, CallStatus}

// HandleCall answers one internal call with the raw JSON body. The caller (the
// leader) was authorized by the internal API; the same function serves the leader's
// calls to itself.
func (a *Agent) HandleCall(ctx context.Context, key string, body []byte) (res any, err error) {
	defer func() {
		if err != nil {
			a.Log.Warn("standby: a call failed", "call", key, "error", err)
		}
	}()
	bad := func(err error) error {
		return &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: err.Error()}
	}
	switch key {
	case CallPlan:
		var r PlanRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, bad(errors.New("the body is not a standby plan"))
		}
		if err := a.Plan(ctx, r); err != nil {
			return nil, bad(err)
		}
		return struct{}{}, nil
	case CallCreate:
		var r CreateRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, bad(errors.New("the body is not a create request"))
		}
		res, err := a.Create(ctx, r.Entry)
		return res, mapErr(err)
	case CallRemove:
		var r RemoveRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, bad(errors.New("the body is not a remove request"))
		}
		return struct{}{}, mapErr(a.Remove(ctx, r.Entry))
	case CallRebuild:
		var r RebuildRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, bad(errors.New("the body is not a rebuild request"))
		}
		res, err := a.Rebuild(ctx, r.Entry, r.Diverged)
		return res, mapErr(err)
	case CallStatus:
		return a.Status(ctx), nil
	case CallFreeze, CallUnfreeze, CallStandbysGone:
		return a.handleFreezeCall(ctx, key, body)
	}
	return nil, &internalapi.CallError{Status: http.StatusNotFound, Code: "not_found", Message: "No such call."}
}

// mapErr turns an error of an operation into the answer of the call: the text of
// an error is never relayed (it may name paths); the log of the receiving server
// has it.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrConflict):
		return &internalapi.CallError{Status: http.StatusConflict, Code: "conflict", Message: "This server already runs another standby of the group."}
	default:
		return err
	}
}
