// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// This file is the side of a server that is being joined to a cluster or
// is a member of one: what the leader's calls (package membership) do here.
// Every operation is idempotent so that the leader's tasks can repeat them.

// ErrNotJoinable means the server cannot be joined now.
var ErrNotJoinable = errors.New("this server is not an uninitialized server that can join a cluster")

// ErrBusy means a setup task is running on this server.
var ErrBusy = errors.New("a setup task is running on this server")

// ServerHost is this server's id (the host name it registers in the catalog).
func (s *Service) ServerHost() string { return s.cfg.ServerHost }

// PGPort is the port of this server's instance.
func (s *Service) PGPort() int { return s.cfg.PGPort }

func (s *Service) fileExists(name string) bool {
	_, err := os.Stat(filepath.Join(s.PKIDir(), name))
	return err == nil
}

// Joining reports whether this server is not part of a cluster yet but holds
// a certificate of one: a join is under way.
func (s *Service) Joining() bool {
	return s.Enabled() && s.setup.State() == setup.Uninitialized && s.fileExists(pki.ServerCertFile) && s.fileExists(pki.CACertFile)
}

// SetupBusy reports whether an init or join task of this server is unfinished.
func (s *Service) SetupBusy(ctx context.Context) bool {
	if s.initStore == nil {
		return false
	}
	ts, err := s.initStore.List(ctx, 20)
	if err != nil {
		return true
	}
	for _, t := range ts {
		if t.State.Active() {
			return true
		}
	}
	return false
}

// PrepareMember is the first step of being joined: the server creates its
// key (once; it never leaves this directory) and a certificate request for
// its server id. internalPort is the port of its internal API.
func (s *Service) PrepareMember(version string) (wire.PrepareResponse, error) {
	if !s.Enabled() {
		return wire.PrepareResponse{}, ErrDisabled
	}
	if s.setup.State() != setup.Uninitialized {
		return wire.PrepareResponse{}, ErrNotJoinable
	}
	s.memberMu.Lock()
	defer s.memberMu.Unlock()
	dir := s.PKIDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return wire.PrepareResponse{}, err
	}
	keyPath := filepath.Join(dir, pki.ServerKeyFile)
	var csr []byte
	if key, err := os.ReadFile(keyPath); err == nil { //nolint:gosec // fixed name below the pki directory
		if csr, err = pki.CSRFromKey(key, s.ServerNames()); err != nil {
			return wire.PrepareResponse{}, err
		}
	} else {
		key, c, err := pki.NewKeyAndCSR(s.ServerNames())
		if err != nil {
			return wire.PrepareResponse{}, err
		}
		if err := pki.WriteFile(keyPath, key, 0o600); err != nil {
			return wire.PrepareResponse{}, err
		}
		csr = c
	}
	return wire.PrepareResponse{ServerID: s.cfg.ServerHost, PGPort: s.cfg.PGPort, InternalPort: s.cfg.InternalPort, CSR: string(csr),
		Version: version, NodeState: string(s.setup.State()), InternalVersion: "1"}, nil
}

// InstallMember stores the cluster CA certificate and this server's
// certificate after checking that the certificate is signed by that CA, names
// this server and belongs to the key this server generated.
func (s *Service) InstallMember(caPEM, certPEM []byte) error {
	if s.setup.State() != setup.Uninitialized {
		return ErrNotJoinable
	}
	s.memberMu.Lock()
	defer s.memberMu.Unlock()
	dir := s.PKIDir()
	key, err := os.ReadFile(filepath.Join(dir, pki.ServerKeyFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return errors.New("this server has not generated a key for a join")
	}
	if old, err := os.ReadFile(filepath.Join(dir, pki.CACertFile)); err == nil && string(old) != string(caPEM) { //nolint:gosec // fixed name
		return errors.New("this server already trusts another cluster CA")
	}
	if err := pki.CheckServerCertificate(caPEM, certPEM, key, s.cfg.ServerHost, time.Now()); err != nil {
		return err
	}
	if err := pki.WriteFile(filepath.Join(dir, pki.CACertFile), caPEM, 0o644); err != nil { //nolint:gosec // a certificate is public
		return err
	}
	if err := pki.WriteFile(filepath.Join(dir, pki.ServerCertFile), certPEM, 0o644); err != nil { //nolint:gosec // a certificate is public
		return err
	}
	_ = os.Remove(filepath.Join(dir, pki.BootstrapCertFile))
	_ = os.Remove(filepath.Join(dir, pki.BootstrapKeyFile))
	return nil
}

// InstallPKI stores the revocation list (and a renewed certificate when one is
// given) and makes a running instance re-read them.
func (s *Service) InstallPKI(ctx context.Context, req wire.PKIRequest) error {
	dir := s.PKIDir()
	have, err := os.ReadFile(filepath.Join(dir, pki.CACertFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil || string(have) != req.CACert {
		return errors.New("the CA certificate is not the one this server trusts")
	}
	ca, err := pki.ParseCertPEM(have)
	if err != nil {
		return err
	}
	if err := checkCRL([]byte(req.CRL), ca); err != nil {
		return err
	}
	// A revocation list never goes back: a list that lacks a revocation this server holds, or has a lower number, is refused and
	// the sender (a leader that was away) merges what the servers hold and sends again. This is checked before anything is written.
	held, _ := os.ReadFile(filepath.Join(dir, pki.CRLFile)) //nolint:gosec // fixed name below the pki directory
	if err := pki.CheckNotStale(held, []byte(req.CRL)); err != nil {
		return err
	}
	if req.ServerCert != "" {
		key, err := os.ReadFile(filepath.Join(dir, pki.ServerKeyFile)) //nolint:gosec // fixed name below the pki directory
		if err != nil {
			return err
		}
		if err := pki.CheckServerCertificate(have, []byte(req.ServerCert), key, s.cfg.ServerHost, time.Now()); err != nil {
			return err
		}
		if err := pki.WriteFile(filepath.Join(dir, pki.ServerCertFile), []byte(req.ServerCert), 0o644); err != nil { //nolint:gosec // a certificate is public
			return err
		}
	}
	if err := pki.WriteFile(filepath.Join(dir, pki.CRLFile), []byte(req.CRL), 0o644); err != nil { //nolint:gosec // a revocation list is public
		return err
	}
	heldList, _ := pki.LoadRevokedList(dir)
	if err := pki.SaveRevokedList(dir, pki.UnionSerials(req.Revoked, heldList)); err != nil {
		return err
	}
	// A server that holds the CA key (a former leader) takes the revocations into its own list as well: it verifies peers with it
	// and would distribute it if it became the leader again.
	if ca, err := s.CA(); err == nil {
		if _, err := ca.MergeRevoked(req.Revoked, time.Now()); err != nil {
			return err
		}
	}
	if s.mgr != nil && s.mgr.Exists(NodeID) {
		return s.reloadAll(ctx)
	}
	return nil
}

func checkCRL(crlPEM []byte, ca *x509.Certificate) error {
	blk, _ := pem.Decode(crlPEM)
	if blk == nil || blk.Type != "X509 CRL" {
		return errors.New("the revocation list is not PEM")
	}
	crl, err := x509.ParseRevocationList(blk.Bytes)
	if err != nil {
		return err
	}
	return crl.CheckSignatureFrom(ca)
}

// PublishLocalCRL writes the revocation list of the CA this server holds to
// the file the local instance reads and reloads the instance.
func (s *Service) PublishLocalCRL(ctx context.Context) error {
	ca, err := s.CA()
	if err != nil {
		return err
	}
	crl, err := ca.CRL(time.Now())
	if err != nil {
		return err
	}
	if err := pki.WriteFile(filepath.Join(s.PKIDir(), pki.CRLFile), crl, 0o644); err != nil { //nolint:gosec // a revocation list is public
		return err
	}
	if s.mgr != nil && s.mgr.Exists(NodeID) {
		return s.reloadAll(ctx)
	}
	return nil
}

// CRLPEM returns the current revocation list of the CA this server holds.
func (s *Service) CRLPEM() ([]byte, error) {
	ca, err := s.CA()
	if err != nil {
		return nil, err
	}
	return ca.CRL(time.Now())
}

func (s *Service) pkiPath(name string) string {
	abs, err := filepath.Abs(filepath.Join(s.PKIDir(), name))
	if err != nil {
		abs = filepath.Join(s.PKIDir(), name)
	}
	return filepath.ToSlash(abs)
}

// withTLS adds the settings that make the instance use the cluster
// certificate (design D3): ssl with this server's certificate, the CA to verify
// client certificates and the revocation list, and the options of the node
// connections of the sharding extension.
func (s *Service) withTLS(m instance.ManagedSettings) (instance.ManagedSettings, error) {
	m.TLSCertFile, m.TLSKeyFile, m.TLSCAFile = s.pkiPath(pki.ServerCertFile), s.pkiPath(pki.ServerKeyFile), s.pkiPath(pki.CACertFile)
	crl := ""
	if s.fileExists(pki.CRLFile) {
		crl = s.pkiPath(pki.CRLFile)
	}
	m.TLSCRLFile = crl
	ci, err := instance.NodeConninfo(m.TLSCertFile, m.TLSKeyFile, m.TLSCAFile, crl)
	if err != nil {
		return m, err
	}
	m.NodeConninfo = ci
	return m, nil
}

// resolveCIDRs returns the networks connections from host arrive from. A
// loopback address also adds the loopback source addresses: on one machine
// every connection between instances comes from 127.0.0.1 whatever address the
// target was given (a cluster on one machine, as the tests use it).
func resolveCIDRs(ctx context.Context, host string) ([]string, error) {
	var ips []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		ips = append(ips, a.Unmap())
	} else {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("the host name %q cannot be resolved", truncName(host))
		}
		for _, a := range addrs {
			if x, ok := netip.AddrFromSlice(a.IP); ok {
				ips = append(ips, x.Unmap())
			}
		}
	}
	var out []string
	loop := false
	for _, a := range ips {
		out = append(out, netip.PrefixFrom(a, a.BitLen()).String())
		loop = loop || a.IsLoopback()
	}
	if loop {
		out = append(out, "127.0.0.1/32", "::1/128")
	}
	return out, nil
}

func (s *Service) identRefresh() error {
	s.proxyMu.Lock()
	f := s.identRef
	s.proxyMu.Unlock()
	if f != nil {
		return f()
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	return s.mgr.WriteIdent(NodeID, u.Username, nil)
}

// SetIdentRefresh registers the function that rewrites pg_ident.conf with the
// roles the proxy currently maps (the node map is added by the manager).
func (s *Service) SetIdentRefresh(f func() error) {
	s.proxyMu.Lock()
	s.identRef = f
	s.proxyMu.Unlock()
}

// ApplyTrust sets the servers this instance trusts, its managed rules and its
// TLS settings, rewrites pg_hba.conf, pg_ident.conf and the managed
// configuration, and reloads a running instance. It is how the leader keeps
// the managed rules of every server in step with the cluster (design D3).
func (s *Service) ApplyTrust(ctx context.Context, req wire.TrustRequest) error {
	if !s.Enabled() || !s.mgr.Exists(NodeID) {
		return errors.New("this server has no instance")
	}
	var trust instance.NodeTrust
	for _, p := range req.Peers {
		cidrs, err := resolveCIDRs(ctx, p.ServerID)
		if err != nil {
			return err
		}
		trust.Peers = append(trust.Peers, instance.NodePeer{ServerID: p.ServerID, CIDRs: cidrs})
	}
	for _, id := range s.ruleIDs() {
		if err := s.mgr.SetTrust(id, trust); err != nil {
			return err
		}
	}
	cur, err := s.mgr.Settings(NodeID)
	if errors.Is(err, os.ErrNotExist) {
		// An instance made before the settings were recorded: start from the
		// settings this server is configured with.
		cur, err = instance.ManagedSettings{Port: s.cfg.PGPort, ListenAddresses: s.cfg.PGListen, ClusterName: s.setup.Snapshot().ClusterName}, nil
	}
	if err != nil {
		return fmt.Errorf("reading the instance settings: %w", err)
	}
	next, err := s.withTLS(cur)
	if err != nil {
		return err
	}
	for _, id := range s.ruleIDs() {
		// The managed settings of an instance hold its own port and its own recovery settings: only the
		// certificate paths and the ones the cluster sets are taken from the home instance's.
		w := next
		if id != NodeID {
			curID, err := s.mgr.Settings(id)
			if err != nil {
				return fmt.Errorf("reading the settings of instance %d: %w", id, err)
			}
			w, err = s.withTLS(curID)
			if err != nil {
				return err
			}
		}
		if err := s.mgr.WriteManaged(id, w); err != nil {
			return err
		}
		if err := s.mgr.WriteHBA(id, HBARules(req.Rules), req.TLSRequired); err != nil {
			return err
		}
	}
	if err := s.identRefresh(); err != nil {
		return err
	}
	return s.reloadAll(ctx)
}

// CreateInstance creates this server's instance (once) with the cluster's
// settings and managed rules, trusts the other servers and starts it. The
// server must hold a certificate of the cluster (Joining).
func (s *Service) CreateInstance(ctx context.Context, req wire.CreateInstanceRequest) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	if !s.Joining() && s.setup.State() == setup.Uninitialized {
		return errors.New("this server has no cluster certificate yet")
	}
	if !s.mgr.Exists(NodeID) {
		pw, err := s.credentials()
		if err != nil {
			return fmt.Errorf("creating the service credentials: %w", err)
		}
		mb := req.MemoryMegabytes
		if mb < 512 {
			mb = 512
		}
		settings, err := s.withTLS(instance.ManagedSettings{
			Port: s.cfg.PGPort, ListenAddresses: s.cfg.PGListen, ClusterName: req.ClusterName, MemoryMegabytes: mb,
			Extra: map[string]string{
				"shared_buffers":       itoa(mb/4) + "MB",
				"effective_cache_size": itoa(mb/2) + "MB",
				"maintenance_work_mem": itoa(min(mb/16, 2048)) + "MB",
			},
		})
		if err != nil {
			return err
		}
		if err := s.mgr.Init(ctx, instance.Spec{ID: NodeID, SuperuserPassword: pw, HBA: HBARules(req.Rules),
			TLSRequired: req.TLSRequired, Settings: settings}); err != nil {
			return fmt.Errorf("initializing the instance: %w", err)
		}
	}
	if err := s.ApplyTrust(ctx, wire.TrustRequest{Peers: req.Peers, Rules: req.Rules, TLSRequired: req.TLSRequired}); err != nil {
		return err
	}
	if err := s.mgr.Start(ctx, NodeID); err != nil {
		return fmt.Errorf("starting the instance: %w", err)
	}
	return s.waitReady(ctx)
}

// InstanceStatus reports whether the instance exists and runs.
func (s *Service) InstanceStatus(ctx context.Context) wire.InstanceStatus {
	st := wire.InstanceStatus{NodeState: string(s.setup.State())}
	if s.Enabled() && s.mgr.Exists(NodeID) {
		st.Exists = true
		st.Running, _ = s.mgr.Running(ctx, s.WritableIDs()[0])
	}
	return st
}

// StartInstance starts the instance of this server.
func (s *Service) StartInstance(ctx context.Context) error {
	if !s.Enabled() || !s.mgr.Exists(NodeID) {
		return errors.New("this server has no instance")
	}
	for _, id := range s.WritableIDs() {
		if err := s.mgr.Start(ctx, id); err != nil {
			return err
		}
	}
	return s.waitReady(ctx)
}

// StopInstance stops the instance; it is StopInstance of the shutdown path.

// RemoveInstance stops and deletes this server's instance and its credentials.
func (s *Service) RemoveInstance(ctx context.Context) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	// The standbys this server runs for other servers' groups go with it: a server that
	// is removed or reset must leave no PostgreSQL process behind.
	for _, id := range s.mgr.IDs() {
		if id != NodeID {
			if err := s.mgr.Remove(ctx, id); err != nil {
				return err
			}
		}
	}
	if err := s.mgr.Remove(ctx, NodeID); err != nil {
		return err
	}
	if err := os.Remove(s.credentialsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// FinishMember makes this server a member: its node state becomes clustered,
// which also removes the setup token (design D4 step 5). Repeating it is not
// an error.
func (s *Service) FinishMember(clusterID, clusterName string) error {
	if s.setup.State() == setup.Clustered {
		return nil
	}
	host := s.cfg.ServerHost
	// The state becomes "clustered" last: a client that sees it signs in and the next request
	// re-checks its role with the service connection, which must by then be the one of the
	// instance this join created (a server that joins again has new service credentials; the
	// old connection settings fail the password check).
	mctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.MigrateSystem(mctx); err != nil {
		s.log.Error("preparing the system database of the new member", "error", err)
	}
	if d := s.cfg.Fault.FinishDelay(); d > 0 {
		s.log.Info("the fault hook holds the end of a join", "for", d.String())
		time.Sleep(d)
	}
	s.publishDSN()
	return s.setup.Transition(setup.Clustered, clusterID, clusterName, &host)
}

// ResetMember returns a server to the state of a fresh install (design D5 and
// the clean-up of a failed join): its instance and credentials are deleted,
// its certificates and key too, it forgets the cluster and gets a new setup
// token.
func (s *Service) ResetMember(ctx context.Context) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	s.resetting.Store(true)
	defer s.resetting.Store(false)
	if err := s.RemoveInstance(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(s.PKIDir()); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.cfg.DataDir, filepath.FromSlash(AdminKeysCacheFile))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.setup.Reset()
}

// Resetting tells whether ResetMember is running. Between the deletion of the certificates and the
// change of the state the server is a member without a certificate; the internal API presents its
// bootstrap certificate for that time (see internalCertificate in cmd/kingresd).
func (s *Service) Resetting() bool { return s.resetting.Load() }

// ApplyRules writes the managed network rules (with the certificate rules for
// the other servers) to pg_hba.conf and reloads the instance.
func (s *Service) ApplyRules(ctx context.Context, rules []api.NetworkAccessRule, tlsRequired bool) error {
	if !s.Enabled() || !s.mgr.Exists(NodeID) {
		return errors.New("this server has no instance")
	}
	for _, id := range s.ruleIDs() {
		if err := s.mgr.WriteHBA(id, HBARules(rules), tlsRequired); err != nil {
			return err
		}
	}
	return s.reloadAll(ctx)
}

// AdminKeysCacheFile is the path, from the data directory, of the cache of the administrators' keys that kingresd keeps for the declaration that a server is lost
// (cmd/kingresd, adminkeys.go). It holds the keys of the cluster that the server belongs to, so it goes with the server's membership.
const AdminKeysCacheFile = "failover/admin-keys.json"
