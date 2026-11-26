// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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
	// CallCredentialsList asks a holder which files it has; CallCredentialsFetch asks it for one (P7A-03). Both are LeaderOnly too: the
	// leader that lacks a file, or wants to know which holders lack one, is the caller.
	CallCredentialsList  = "POST /ledger/anchor/credentials/list"
	CallCredentialsFetch = "POST /ledger/anchor/credentials/fetch"
)

// Calls lists the keys of the internal calls the agent answers.
var Calls = []string{CallCredentialsInstall, CallCredentialsRemove, CallCredentialsList, CallCredentialsFetch}

// ListResponse is the answer of CallCredentialsList: the target ids that the server has a file for, and for each file that can be opened
// its rotation generation and the keyed digest of what it holds (P7F-01; an older server sends no copies, and the leader then cannot tell a stale file).
type ListResponse struct {
	IDs    []int64    `json:"ids"`
	Copies []CopyInfo `json:"copies,omitempty"`
}

// FetchRequest is the body of CallCredentialsFetch; FetchResponse its answer, the file as the holder has it (standard base64).
type FetchRequest struct {
	TargetID int64 `json:"targetId"`
}

// FetchResponse is the answer of CallCredentialsFetch.
type FetchResponse struct {
	File string `json:"file"`
}

// InstallRequest is the body of CallCredentialsInstall. File is the standard base64 of the credentials file.
type InstallRequest struct {
	TargetID int64  `json:"targetId"`
	Kind     string `json:"kind"`
	File     string `json:"file"`
	// Known is the highest generation that the leader saw at the holders: the reference of the bound for a holder that has no file (P7I-01).
	Known uint64 `json:"known,omitempty"`
}

type knownKey struct{}

// WithKnownGeneration carries the highest generation seen at the holders to the function that hands a file over.
func WithKnownGeneration(ctx context.Context, g uint64) context.Context {
	return context.WithValue(ctx, knownKey{}, g)
}

// KnownGeneration is what WithKnownGeneration carried (0 when nothing).
func KnownGeneration(ctx context.Context) uint64 {
	g, _ := ctx.Value(knownKey{}).(uint64)
	return g
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
		if err := a.Creds.InstallFrom(in.TargetID, in.Kind, raw, InstallSource{Leader: true, Known: in.Known}); err != nil {
			if errors.Is(err, ErrGenerationJump) {
				return nil, bad("generation_too_far", "The generation of the file is far above the one of this server: if this server's file is damaged, remove it (<key directory>/anchor/<id>.json) and the next pass gives it the leader's file.", http.StatusConflict)
			}
			if errors.Is(err, ErrOlderGeneration) {
				return nil, bad("older_generation", "This server holds a newer credentials file for the target, or a different one of the same generation.", http.StatusConflict)
			}
			if errors.Is(err, ErrInstallWrite) {
				return nil, bad("credentials_not_written", "The credentials file could not be written on this server.", http.StatusInternalServerError)
			}
			if errors.Is(err, ErrCredentials) {
				return nil, bad("passphrase_mismatch", "The credentials file cannot be opened with this server's passphrase file.", http.StatusConflict)
			}
			return nil, bad("credentials_invalid", "The credentials file was refused.", http.StatusUnprocessableEntity)
		}
		return struct{}{}, nil
	case CallCredentialsList:
		ids, err := a.Creds.IDs()
		if err != nil {
			return nil, bad("credentials_invalid", "The credentials directory could not be read.", http.StatusUnprocessableEntity)
		}
		if ids == nil {
			ids = []int64{}
		}
		out := ListResponse{IDs: ids}
		for _, id := range ids {
			if c, err := a.Creds.Digest(id); err == nil {
				out.Copies = append(out.Copies, c)
			}
		}
		return out, nil
	case CallCredentialsFetch:
		var in FetchRequest
		if err := json.Unmarshal(body, &in); err != nil || in.TargetID < 1 {
			return nil, bad("bad_request", "The body is not valid.", http.StatusBadRequest)
		}
		raw, err := a.Creds.Raw(in.TargetID)
		switch {
		case errors.Is(err, ErrNoCredentials):
			return nil, bad("not_found", FetchNoneMessage, http.StatusNotFound)
		case err != nil:
			return nil, bad("credentials_invalid", "The credentials file could not be read.", http.StatusUnprocessableEntity)
		}
		defer clear(raw)
		return FetchResponse{File: base64.StdEncoding.EncodeToString(raw)}, nil
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

// FetchNoneMessage is the text of the 404 answer of a holder that has no credentials file for the target. A 404 with another text (a server that
// does not know the call) does not say that the holder has none (P7R-08).
const FetchNoneMessage = "This server has no credentials file for the target."
