// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// TestInternalAPIOfARunningServer: after init-cluster the server has a CA and a
// certificate, and its internal listener answers a caller that presents it (the
// server is its own leader), and nobody else.
func TestInternalAPIOfARunningServer(t *testing.T) {
	e := shared(t)
	dir := filepath.Join(e.dataDir, "pki")
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("no CA after init-cluster: %v", err)
	}
	if st, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil || st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("CA key: %v %v", st, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string, cert *tls.Certificate, version string) (int, map[string]any, error) {
		tc := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "127.0.0.1"}
		if cert != nil {
			tc.Certificates = []tls.Certificate{*cert}
		}
		cl := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 10 * time.Second}
		req, _ := http.NewRequest("GET", "https://"+e.internal+internalapi.Prefix+path, nil)
		req.Header.Set(internalapi.HeaderVersion, version)
		resp, err := cl.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m, nil
	}
	// The listener starts shortly after the certificate exists.
	var st int
	var out map[string]any
	for i := 0; i < 30; i++ {
		if st, out, err = get("/state", &pair, internalapi.Version); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil || st != 200 || out["nodeState"] != "clustered" || out["internalVersion"] != internalapi.Version {
		t.Fatalf("state: %d %v %v", st, out, err)
	}
	if st, _, err := get("/resources", &pair, internalapi.Version); err != nil || st != 200 {
		t.Errorf("the leader (this server) asks for resources: %d %v", st, err)
	}
	if st, out, err := get("/state", nil, internalapi.Version); err == nil && (st != 401 || out["error"] != "no_certificate") {
		t.Errorf("a call without a certificate was answered: %d %v", st, out)
	}
	if st, out, err := get("/state", &pair, "99"); err != nil || st != 409 || out["error"] != "version_mismatch" {
		t.Errorf("version: %d %v %v", st, out, err)
	}
	// A certificate of another CA is refused.
	other, err := pki.CreateCA(filepath.Join(tempDir(t), "pki"), "other", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n := pki.Names{CommonName: "127.0.0.1"}
	key, csr, _ := pki.NewKeyAndCSR(n)
	crt, _, _ := other.Sign(csr, n, time.Now())
	op, _ := tls.X509KeyPair(crt, key)
	if st, _, err := get("/state", &op, internalapi.Version); err == nil && st == 200 {
		t.Error("a certificate of another CA was accepted")
	}
	// The public API does not take the certificate as a login, and the internal
	// API takes no session: a session token on the internal listener gets nothing.
	req, _ := http.NewRequest("GET", "https://"+e.internal+internalapi.Prefix+"/state", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set(internalapi.HeaderVersion, internalapi.Version)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}}
	if resp, err := cl.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("a session token was accepted by the internal API: %d", resp.StatusCode)
		}
	}
}
