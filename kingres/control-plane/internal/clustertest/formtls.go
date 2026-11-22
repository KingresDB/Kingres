// SPDX-License-Identifier: AGPL-3.0-only

package clustertest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FormTLS is FormWithExtra for a cluster whose servers serve HTTPS on their public listeners (the data transfer refuses plain HTTP), each with a
// certificate made for the test. extra are further flags of every node.
func FormTLS(t testing.TB, n int, extra []string, before func(leader *Node)) *Cluster {
	t.Helper()
	c := New(t, n)
	for _, nd := range c.Nodes {
		crt, key := selfSigned(t, nd)
		nd.TLS = true
		nd.Extra = append([]string{"-tls-cert-file", crt, "-tls-key-file", key}, extra...)
		nd.client = &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 4}} //nolint:gosec // the node under test, on loopback
	}
	for _, nd := range c.Nodes {
		nd.Start()
	}
	c.Leader().InitCluster("multi")
	if before != nil {
		before(c.Leader())
	}
	c.joinRest(t, 1)
	return c
}

func selfSigned(t testing.TB, nd *Node) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(int64(nd.Index) + 11), Subject: pkix.Name{CommonName: nd.Host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP(nd.Host)}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(nd.Dir, "public.crt"), filepath.Join(nd.Dir, "public.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// DoBody sends a request with a binary body and returns the status, the headers and the body of the answer.
func (n *Node) DoBody(method, path string, body []byte, headers map[string]string) (int, http.Header, []byte, error) {
	req, _ := http.NewRequest(method, n.Base()+path, bytesReader(body))
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := readAll(resp.Body)
	return resp.StatusCode, resp.Header, b, err
}

func bytesReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
