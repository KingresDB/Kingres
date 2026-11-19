// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// The internal calls of the credentials of anchor targets (internal-api.md, "The credentials of anchor targets"). Both
// are LeaderOnly: the leader gives a holder the credentials file of a target, and takes it away. The file in the
// body is the one the leader holds (clear or sealed), never a passphrase; the callee opens it with its own
// passphrase file and writes it again under its own sealing, as for a signing key (correction 131).
const (
	CallCredentialsInstall = "POST /ledger/anchor/credentials/install"
	CallCredentialsRemove  = "POST /ledger/anchor/credentials/remove"
)

// Calls lists the keys of the internal calls the agent answers.
var Calls = []string{CallCredentialsInstall, CallCredentialsRemove}

// InstallRequest is the body of CallCredentialsInstall. File is the standard base64 of the credentials file.
type InstallRequest struct {
	TargetID int64  `json:"targetId"`
	Kind     string `json:"kind"`
	File     string `json:"file"`
}

// RemoveRequest is the body of CallCredentialsRemove.
type RemoveRequest struct {
	TargetID int64 `json:"targetId"`
}

// Agent answers the calls on a server.
type Agent struct{ Creds *CredStore }

func bad(code, msg string, status int) error {
	return &internalapi.CallError{Status: status, Code: code, Message: msg}
}

// Handle runs one call; the body is raw JSON. No answer and no error text holds a credential: the text says why the
// file was refused and never repeats a member of it.
func (a *Agent) Handle(_ context.Context, key string, body []byte) (any, error) {
	switch key {
	case CallCredentialsInstall:
		var in InstallRequest
		if err := json.Unmarshal(body, &in); err != nil || in.TargetID < 1 {
			return nil, bad("bad_request", "The body is not valid.", http.StatusBadRequest)
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(in.File)
		in.File = ""
		if err != nil {
			return nil, bad("bad_request", "The file is not valid base64.", http.StatusBadRequest)
		}
		defer clear(raw)
		if _, err := kindOf(in.Kind); err != nil {
			return nil, bad("bad_request", "The kind is not known on this server.", http.StatusBadRequest)
		}
		if err := a.Creds.Install(in.TargetID, in.Kind, raw); err != nil {
			if errors.Is(err, ErrCredentials) {
				return nil, bad("passphrase_mismatch", "The credentials file cannot be opened with this server's passphrase file.", http.StatusConflict)
			}
			return nil, bad("credentials_invalid", "The credentials file was refused.", http.StatusUnprocessableEntity)
		}
		return struct{}{}, nil
	case CallCredentialsRemove:
		var in RemoveRequest
		if err := json.Unmarshal(body, &in); err != nil || in.TargetID < 1 {
			return nil, bad("bad_request", "The body is not valid.", http.StatusBadRequest)
		}
		if err := a.Creds.Remove(in.TargetID); err != nil {
			return nil, bad("credentials_invalid", "The credentials file could not be removed.", http.StatusUnprocessableEntity)
		}
		return struct{}{}, nil
	}
	return nil, bad("not_found", "No such call.", http.StatusNotFound)
}
