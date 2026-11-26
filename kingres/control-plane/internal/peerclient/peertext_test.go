// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package peerclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// Class search of 2026-11-14: the text a peer answers with ends up in problems,
// task errors and logs; it must be short and free of control characters.
func TestPeerErrorTextIsBoundedAndClean(t *testing.T) {
	hostile := "line one\r\nforged: yes \x1b[31mred\x1b[0m " + strings.Repeat("A", 100000)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		b, _ := jsonMarshal(map[string]string{"error": "bad\ncode", "message": hostile})
		_, _ = w.Write(b)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := BootstrapCall(ctx, strings.TrimPrefix(srv.URL, "https://"), "", "POST", "/x", nil, nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("not an APIError: %v", err)
	}
	if len(ae.Message) > MaxPeerText+8 || len(ae.Code) > 64 {
		t.Errorf("unbounded peer text: %d / %d bytes", len(ae.Message), len(ae.Code))
	}
	for _, s := range []string{ae.Message, ae.Code, ae.Error()} {
		for _, r := range s {
			if unicode.IsControl(r) || r == ' ' {
				t.Fatalf("control character %U in %q", r, s)
			}
		}
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// The body of a gather call has no proof, whatever the caller put in the request:
// a proof would reveal the user's ClientKey to the member that receives it.
func TestGatherBodyHasNoProof(t *testing.T) {
	req := internalapi.ForwardRequest{OperationID: "listLogs", Identity: internalapi.ForwardIdentity{Username: "alice", ProofKey: []byte("secret-key-material-0123456789ab")},
		Proof: &fwdproof.Proof{IssuedAt: 1, Nonce: make([]byte, 16), Value: make([]byte, 32)}, Assertion: &fwdproof.Assertion{Leader: []byte{1}}}
	b, err := gatherBody(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"proof"`, "proofKey", "ProofKey", "secret-key"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("the gather body contains %q: %s", forbidden, b)
		}
	}
	if !strings.Contains(string(b), `"assertion"`) {
		t.Errorf("the gather body has no assertion: %s", b)
	}
}
