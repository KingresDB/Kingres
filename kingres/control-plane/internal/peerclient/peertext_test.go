// SPDX-License-Identifier: PostgreSQL

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
