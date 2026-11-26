// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgersign

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// The internal calls of the signing keys (internal-api.md, "The ledger keys"). All three are LeaderOnly: the
// leader gives a holder the key file, takes it away, and asks what a server has. The file in the body is the
// one the leader holds (plain or sealed), never a passphrase; the callee opens it with its own passphrase file
// and writes it again under its own sealing.
const (
	CallInstall = "POST /ledger/key/install"
	CallRemove  = "POST /ledger/key/remove"
	CallList    = "POST /ledger/key/list"
)

// Calls lists the keys of the internal calls the agent answers.
var Calls = []string{CallInstall, CallRemove, CallList}

// InstallRequest is the body of CallInstall. File is the standard base64 of the key file.
type InstallRequest struct {
	Fingerprint string `json:"fingerprint"`
	File        string `json:"file"`
}

// RemoveRequest is the body of CallRemove.
type RemoveRequest struct {
	Fingerprint string `json:"fingerprint"`
}

// ListResponse is the answer of CallList: the fingerprints of the key files on the server, no more.
type ListResponse struct {
	Fingerprints []string `json:"fingerprints"`
	// Unopenable are the key files that exist and do not open with the passphrase of this server (garbage, another passphrase, a truncated file).
	Unopenable []string `json:"unopenable,omitempty"`
}

// Agent answers the calls on a server.
type Agent struct{ Keyring *Keyring }

func bad(code, msg string, status int) error {
	return &internalapi.CallError{Status: status, Code: code, Message: msg}
}

// Handle runs one call; the body is raw JSON.
func (a *Agent) Handle(_ context.Context, key string, body []byte) (any, error) {
	switch key {
	case CallInstall:
		var in InstallRequest
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, bad("bad_request", "The body is not valid.", http.StatusBadRequest)
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(in.File)
		in.File = ""
		if err != nil {
			return nil, bad("bad_request", "The file is not valid base64.", http.StatusBadRequest)
		}
		defer clear(raw)
		switch err := a.Keyring.Install(in.Fingerprint, raw); {
		case err == nil:
			return struct{}{}, nil
		case errors.Is(err, ErrPassphrase):
			return nil, bad("passphrase_mismatch", "The key file cannot be opened with this server's passphrase file.", http.StatusConflict)
		default:
			// The text of the error names no secret; it says why the file was refused.
			return nil, bad("key_invalid", err.Error(), http.StatusUnprocessableEntity)
		}
	case CallRemove:
		var in RemoveRequest
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, bad("bad_request", "The body is not valid.", http.StatusBadRequest)
		}
		if err := a.Keyring.Remove(in.Fingerprint); err != nil {
			return nil, bad("key_invalid", err.Error(), http.StatusUnprocessableEntity)
		}
		return struct{}{}, nil
	case CallList:
		a.Keyring.CleanTemp(time.Minute)
		fps, unopenable, err := a.Keyring.ListChecked()
		if err != nil {
			return nil, bad("unavailable", "The key directory cannot be read.", http.StatusServiceUnavailable)
		}
		if fps == nil {
			fps = []string{}
		}
		return ListResponse{Fingerprints: fps, Unopenable: unopenable}, nil
	}
	return nil, bad("not_found", "No such call.", http.StatusNotFound)
}
