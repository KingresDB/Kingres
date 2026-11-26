// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// KindJoin is the task kind of the new server's side of a join.
const KindJoin = "cluster.join"

type joinParams struct {
	// ExistingInternal is host:port of the internal API of the server the
	// administrator named (its REST address with the port above it, see the
	// README).
	ExistingInternal string  `json:"existingInternal"`
	AdminUser        string  `json:"adminUser"`
	Pin              string  `json:"pinnedFingerprint,omitempty"`
	MemoryMB         int     `json:"memoryMegabytes"`
	AdvertiseHost    string  `json:"advertiseHost"`
	ServerGroup      *string `json:"serverGroup,omitempty"`
}

var nodeAddrRE = regexp.MustCompile(`^(\[[0-9a-fA-F:]+\]|[A-Za-z0-9.-]+):([1-9][0-9]{0,4})$`)

// InternalAddressOf returns the address of the internal API of the server whose
// REST API is at restAddr: the same host, the port above (the contract names
// only the REST address; see "Running several servers" in the README).
func InternalAddressOf(restAddr string) (string, error) {
	host, port, err := net.SplitHostPort(restAddr)
	if err != nil || !nodeAddrRE.MatchString(restAddr) {
		return "", fmt.Errorf("%q is not a host:port address", truncName(restAddr))
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n >= 65535 {
		return "", fmt.Errorf("%q has no usable port", truncName(restAddr))
	}
	return net.JoinHostPort(host, strconv.Itoa(n+1)), nil
}

// ValidateJoin checks a join request without touching anything.
func ValidateJoin(req api.JoinClusterRequest, dataDir string) []FieldError {
	var errs []FieldError
	bad := func(f, m, c string) { errs = append(errs, FieldError{f, m, c}) }
	if _, err := InternalAddressOf(req.ExistingNodeAddress); err != nil {
		bad("/existingNodeAddress", "must be host:port of a server's REST API", "pattern")
	}
	if !adminNameRE.MatchString(req.Admin.Username) {
		bad("/admin/username", "must match ^[a-z_][a-z0-9_]{0,62}$", "pattern")
	}
	if l := len(req.Admin.Password); l < 1 || l > 1024 {
		bad("/admin/password", "must be 1 to 1024 characters", "out_of_range")
	}
	want, _ := filepath.Abs(dataDir)
	if got := req.DataPaths.DataDirectory; !filepath.IsAbs(got) || filepath.Clean(got) != filepath.Clean(want) {
		bad("/dataPaths/dataDirectory", "must be the data directory of this server ("+want+")", "mismatch")
	}
	if req.DataPaths.WalDirectory != nil {
		bad("/dataPaths/walDirectory", "a separate WAL directory is not supported yet; use null", "unsupported")
	}
	if req.DataPaths.LogDirectory != nil {
		bad("/dataPaths/logDirectory", "a separate log directory is not supported yet; use null", "unsupported")
	}
	if req.MemoryQuota != nil && (req.MemoryQuota.TotalMegabytes < 512 || req.MemoryQuota.TotalMegabytes > 1<<24) {
		bad("/memoryQuota/totalMegabytes", "must be at least 512", "out_of_range")
	}
	if req.ServerGroup != nil {
		bad("/serverGroup", "server groups are not supported yet; use null", "unsupported")
	}
	if a := req.AdvertiseAddress; a != nil {
		if !nodeAddrRE.MatchString(*a) {
			bad("/advertiseAddress", "must be host:port", "pattern")
		}
	}
	return errs
}

// Join starts the new server's side of a join: it asks a server of the cluster,
// with the administrator's credentials, to add this server, installs the
// certificates it gets, and waits until the leader has finished. pin is the
// SHA-256 fingerprint the administrator expects of the existing server's
// certificate ("" for trust on first use). The password is kept in memory for
// the run of the task only.
func (s *Service) Join(req api.JoinClusterRequest, pin string) (task.Task, error) {
	if !s.Enabled() {
		return task.Task{}, ErrDisabled
	}
	internal, err := InternalAddressOf(req.ExistingNodeAddress)
	if err != nil {
		return task.Task{}, err
	}
	p := joinParams{ExistingInternal: internal, AdminUser: req.Admin.Username, Pin: pin, MemoryMB: 1024, AdvertiseHost: s.cfg.ServerHost}
	if req.MemoryQuota != nil {
		p.MemoryMB = req.MemoryQuota.TotalMegabytes
	}
	if req.AdvertiseAddress != nil {
		if h, _, err := net.SplitHostPort(*req.AdvertiseAddress); err == nil {
			p.AdvertiseHost = h
		}
	}
	t, run, err := s.initEngine.SubmitHeld(context.Background(), KindJoin, task.Subject{Type: "cluster", ID: "cluster"}, p, "setup")
	if err != nil {
		return task.Task{}, err
	}
	secrets := map[string]string{"adminPassword": req.Admin.Password}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := run(s.bg, secrets); err != nil && s.bg.Err() == nil {
			s.log.Error("join task ended with an error", "task", t.ID, "error", err)
		}
	}()
	return t, nil
}

func (s *Service) joinDefinition() task.Definition {
	return task.Definition{Kind: KindJoin, Steps: []task.Step{
		{Name: "Create this server's key and certificate request", Run: s.stepJoinPrepare},
		{Name: "Ask the cluster to add this server", NeedsSecret: "adminPassword", Run: s.stepJoinIntroduce},
		{Name: "Wait for the cluster to finish adding this server", Run: s.stepJoinWait},
	}}
}

func (s *Service) stepJoinPrepare(_ context.Context, _ *task.Env) error {
	_, err := s.PrepareMember(s.cfg.Version)
	return err
}

func (s *Service) stepJoinIntroduce(ctx context.Context, env *task.Env) error {
	if s.Joining() {
		return nil // an earlier run of this step got the certificate
	}
	var p joinParams
	if err := json.Unmarshal(env.Task.Input, &p); err != nil {
		return err
	}
	pw, _ := env.Secret("adminPassword")
	prep, err := s.PrepareMember(s.cfg.Version)
	if err != nil {
		return err
	}
	var resp wire.JoinResponse
	seen, err := peerclient.BootstrapCall(ctx, p.ExistingInternal, p.Pin, "POST", "/bootstrap/join", nil, wire.JoinRequest{
		Username: p.AdminUser, Password: pw, ServerID: prep.ServerID, CSR: prep.CSR, PGPort: prep.PGPort,
		InternalAddress: net.JoinHostPort(p.AdvertiseHost, strconv.Itoa(s.cfg.InternalPort)),
		ServerGroup:     p.ServerGroup, MemoryMegabytes: p.MemoryMB,
	}, &resp)
	if seen != "" {
		mode := "first use, not verified; compare it with the fingerprint shown on the existing server"
		if p.Pin != "" {
			mode = "pinned"
		}
		detail := fmt.Sprintf("Internal API certificate of the existing server, SHA-256 %s (%s).", pki.FormatFingerprint(seen), mode)
		s.log.Info("join: certificate seen on first contact", "sha256", pki.FormatFingerprint(seen), "pinned", p.Pin != "")
		_ = env.SetDetail(ctx, detail)
	}
	if err != nil {
		if errors.Is(err, peerclient.ErrFingerprintMismatch) {
			return errors.New("the certificate of the existing server does not match the pinned fingerprint; nothing was sent")
		}
		var ae *peerclient.APIError
		if errors.As(err, &ae) {
			return fmt.Errorf("the cluster refused the join: %s", ae.Message)
		}
		return fmt.Errorf("the existing server could not be reached: %w", err)
	}
	return s.InstallMember([]byte(resp.CACert), []byte(resp.ServerCert))
}

func (s *Service) stepJoinWait(ctx context.Context, _ *task.Env) error {
	deadline := time.Now().Add(60 * time.Minute)
	for time.Now().Before(deadline) {
		switch {
		case s.setup.State() == setup.Clustered:
			return nil
		case !s.Joining():
			return errors.New("the cluster discarded this server's join; start again with the setup token")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("the cluster did not finish adding this server in time")
}

// Joinable reports whether no setup task is running on this server.
func (s *Service) Joinable() bool { return !s.SetupBusy(context.Background()) }

// VerifyToken checks the setup token of this server.
func (s *Service) VerifyToken(presented string) (bool, error) { return s.setup.VerifyToken(presented) }
