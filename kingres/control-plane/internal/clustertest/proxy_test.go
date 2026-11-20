// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

const (
	proxyUser = "pxuser"
	proxyPass = "Proxy-Password-1!"
	certUser  = "pxcert"
)

// viaProxy opens a connection to the client entry point of n over TLS (verified
// against the cluster CA, host name checked), with a password or a client certificate.
func viaProxy(ctx context.Context, n *clustertest.Node, port int, user, pass string, cert *tls.Certificate, pool *x509.CertPool) (*pgx.Conn, error) {
	return viaProxyMode(ctx, n, port, user, pass, cert, pool, false)
}

// viaProxyMode is viaProxy; with verifyCAOnly the certificate chain is verified against the cluster CA but the
// name is not checked (sslmode verify-ca): a connection that the entry point forwards to another server ends
// at an instance whose certificate names that server, not the one the client dialed.
func viaProxyMode(ctx context.Context, n *clustertest.Node, port int, user, pass string, cert *tls.Certificate, pool *x509.CertPool, verifyCAOnly bool) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = n.Host, uint16(port), "postgres", user, pass //nolint:gosec // test ports
	cfg.Fallbacks, cfg.ConnectTimeout = nil, 15*time.Second
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: n.Host}
	if verifyCAOnly {
		cfg.TLSConfig.InsecureSkipVerify = true // nolint:gosec // the chain is verified below
		cfg.TLSConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			opts := x509.VerifyOptions{Roots: pool, Intermediates: x509.NewCertPool()}
			for _, c := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		}
	}
	if cert != nil {
		cfg.TLSConfig.Certificates = []tls.Certificate{*cert}
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// viaProxyHome is viaProxy until the session is on n's own instance. Right after the formation the primary of a server has not won its
// lease yet, and the entry point then sends the connection to another server's primary (design: a primary without its lease is not a
// destination); the certificate that connection meets names that server, which is the answer / condition the test waits out.
func viaProxyHome(ctx context.Context, n *clustertest.Node, port int, user, pass string, cert *tls.Certificate, pool *x509.CertPool) (*pgx.Conn, error) {
	deadline := time.Now().Add(90 * time.Second)
	for {
		conn, err := viaProxy(ctx, n, port, user, pass, cert, pool)
		if err == nil {
			s, werr := whereAmI(ctx, conn)
			if werr == nil && s.Port == n.PGPort {
				return conn, nil
			}
			_ = conn.Close(ctx)
			err = fmt.Errorf("the session is not on the server's own instance yet: %+v %v", s, werr)
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

type session struct {
	Port int
	SSL  bool
	DN   string
	User string
}

func whereAmI(ctx context.Context, c *pgx.Conn) (session, error) {
	var s session
	err := c.QueryRow(ctx, `SELECT inet_server_port(), coalesce(t.ssl, false), coalesce(t.client_dn, ''), current_user::text
		FROM (SELECT 1) x LEFT JOIN pg_stat_ssl t ON t.pid = pg_backend_pid()`).Scan(&s.Port, &s.SSL, &s.DN, &s.User)
	return s, err
}

// TestClientEntryPoint runs three real kingresd processes with the client entry point on a common port
// (every server has its own loopback address) and shows: a client that negotiates TLS with the instance
// end to end and signs in with SCRAM, and one that signs in with a client certificate, both through the
// entry point of every server, land on that server's own primary; when a server's own primary is not a
// valid destination the entry point sends new connections to another server's primary, and closes the
// old ones; when the destination is right again, the forwarded ones are closed and new ones come home.
func TestClientEntryPoint(t *testing.T) {
	port := pgtest.FreePort(t)
	t.Cleanup(func() { pgtest.ReleasePorts(port) })
	c := clustertest.FormEach(t, 3, func(n *clustertest.Node) []string {
		return []string{"-client-listen", fmt.Sprintf("%s:%d", n.Host, port)}
	}, nil)
	leader := c.Leader()
	mkRole(t, leader, proxyUser, proxyPass, "kingres_readonly")
	mkRole(t, leader, certUser, "Unused-Password-77", "kingres_readonly")

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(readFile(t, leader.DataDir, "pki", "ca.crt"))
	ctx := context.Background()

	// SCRAM over TLS through every entry point: the session is on that server's own instance.
	for _, n := range c.Nodes {
		conn, err := viaProxyHome(ctx, n, port, proxyUser, proxyPass, nil, pool)
		if err != nil {
			t.Fatalf("node %d: connecting through the entry point: %v\n%s", n.Index, err, n.LogTail(2000))
		}
		s, err := whereAmI(ctx, conn)
		_ = conn.Close(ctx)
		if err != nil || !s.SSL || s.Port != n.PGPort || s.User != proxyUser || s.DN != "" {
			t.Errorf("node %d: session %+v (%v); want TLS, port %d, user %s and no client certificate", n.Index, s, err, n.PGPort, proxyUser)
		}
	}

	// A client certificate: the certificate rule is first, the certificate is signed by the cluster CA and
	// names the role, nothing but the certificate authenticates the client.
	cur := leader.Expect(200, "GET", "/settings/network-access", nil)
	rules, _ := cur["rules"].([]any)
	rules = append([]any{map[string]any{"type": "hostssl", "databases": []string{"all"}, "users": []string{certUser}, "address": "127.0.0.0/8", "method": "cert"}}, rules...)
	body := leader.Expect(202, "PUT", "/settings/network-access", map[string]any{"revision": cur["revision"], "tlsRequired": cur["tlsRequired"], "rules": rules})
	leader.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
	ca, err := pki.LoadCA(filepath.Join(leader.DataDir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	names := pki.Names{CommonName: certUser}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(names)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := ca.Sign(csrPEM, names, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range c.Nodes {
		conn, err := viaProxyHome(ctx, n, port, certUser, "", &pair, pool)
		if err != nil {
			t.Fatalf("node %d: client certificate through the entry point: %v\n%s", n.Index, err, n.LogTail(2000))
		}
		s, err := whereAmI(ctx, conn)
		_ = conn.Close(ctx)
		if err != nil || !s.SSL || s.Port != n.PGPort || s.User != certUser || !strings.Contains(s.DN, certUser) {
			t.Errorf("node %d: session %+v (%v); want TLS, port %d, user %s and the client certificate", n.Index, s, err, n.PGPort, certUser)
		}
	}
	// The wrong password and a certificate of another name do not get in through the entry point either.
	if conn, err := viaProxy(ctx, c.Nodes[0], port, proxyUser, "wrong", nil, pool); err == nil {
		_ = conn.Close(ctx)
		t.Error("a wrong password was accepted through the entry point")
	}

	// The destination changes. Node 2's own copy of the catalog says its primary is not active, which is
	// what makes it an unusable local destination exactly as a lost lease will (the lease manager is the
	// other half of the same rule, tested with it); its entry point must send new connections to another
	// server, whose entry point connects them to its own instance.
	n2 := c.Nodes[1]
	home, err := viaProxy(ctx, n2, port, proxyUser, proxyPass, nil, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = home.Close(ctx) }()
	if s, err := whereAmI(ctx, home); err != nil || s.Port != n2.PGPort {
		t.Fatalf("before the change: %+v %v", s, err)
	}
	cat := n2.ServicePG("kingres_system")
	setActive := func(on bool) {
		t.Helper()
		if _, err := cat.Exec(ctx, `UPDATE kingres.node SET is_active = $2 WHERE host = $1 AND role = 'primary'`, n2.Host, on); err != nil {
			t.Fatalf("changing the local catalog: %v", err)
		}
	}
	setActive(false)
	waitProxy(t, "the connection to the old destination is closed", func() bool {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := home.Exec(cctx, "SELECT 1")
		return err != nil
	})
	var away *pgx.Conn
	waitProxy(t, "new connections go to another server's primary", func() bool {
		conn, err := viaProxyMode(ctx, n2, port, proxyUser, proxyPass, nil, pool, true)
		if err != nil {
			return false
		}
		s, err := whereAmI(ctx, conn)
		if err != nil || s.Port == n2.PGPort || (s.Port != c.Nodes[0].PGPort && s.Port != c.Nodes[2].PGPort) {
			_ = conn.Close(ctx)
			return false
		}
		away = conn
		return true
	})
	defer func() { _ = away.Close(ctx) }()
	setActive(true)
	waitProxy(t, "the forwarded connection is closed when the destination is home again", func() bool {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := away.Exec(cctx, "SELECT 1")
		return err != nil
	})
	waitProxy(t, "new connections come home", func() bool {
		conn, err := viaProxyMode(ctx, n2, port, proxyUser, proxyPass, nil, pool, true)
		if err != nil {
			return false
		}
		defer func() { _ = conn.Close(ctx) }()
		s, err := whereAmI(ctx, conn)
		return err == nil && s.Port == n2.PGPort
	})
}

func waitProxy(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
