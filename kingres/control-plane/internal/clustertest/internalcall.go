// SPDX-License-Identifier: PostgreSQL

package clustertest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// InternalCall calls the internal API of target presenting the certificate of n,
// so the call is made as the server n (a test acts as one of the cluster's
// servers: it reads the certificate from n's data directory). in is sent as JSON
// when not nil and the answer decoded into out when not nil. It returns the HTTP
// status; an error means the call did not complete (a dropped connection, for
// example, is what a blocked fault looks like).
func (n *Node) InternalCall(target *Node, method, path string, in, out any) (int, error) {
	pkiDir := filepath.Join(n.DataDir, "pki")
	caPEM, err := os.ReadFile(filepath.Join(pkiDir, pki.CACertFile))
	if err != nil {
		return 0, fmt.Errorf("reading the CA certificate of node %d: %w", n.Index, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	pair, err := tls.LoadX509KeyPair(filepath.Join(pkiDir, pki.ServerCertFile), filepath.Join(pkiDir, pki.ServerKeyFile))
	if err != nil {
		return 0, fmt.Errorf("reading the certificate of node %d: %w", n.Index, err)
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: target.Host, Certificates: []tls.Certificate{pair}},
	}}
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, "https://"+target.InternalAddr()+internalapi.Prefix+path, body)
	req.Header.Set(internalapi.HeaderVersion, internalapi.Version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decoding the answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}
