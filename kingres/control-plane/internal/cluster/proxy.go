// SPDX-License-Identifier: AGPL-3.0-only

package cluster

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgframe"
)

// ErrNotEnabled means the role has not signed in since kingresd started, so
// it cannot be impersonated.
var ErrNotEnabled = errors.New("the role has not signed in")

// Proxy opens connections as signed-in roles without their passwords. The
// design (control-plane-2.md, D6) wants the role's own PostgreSQL permission
// checks to apply to role and grant changes and to the query workbench. A
// connection can only be authenticated with the password, which kingresd
// never keeps, and a connection is bound to one database, so the sign-in
// connection cannot serve every database. Instead PostgreSQL itself
// authenticates kingresd's operating system user over the unix socket
// (peer authentication with a pg_ident.conf map): a role is mapped when it
// signs in with a valid password (Allow), the map is rebuilt empty when
// kingresd starts, and the connections are ordinary sessions of that role, so
// no statement of theirs can gain more than the role has.
type Proxy struct {
	svc    *Service
	osUser string

	mu    sync.Mutex
	roles map[string]struct{}
}

// NewProxy returns the proxy of the service. It fails when the operating
// system user cannot be determined.
func (s *Service) NewProxy() (*Proxy, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("determining the operating system user: %w", err)
	}
	p := &Proxy{svc: s, osUser: u.Username, roles: map[string]struct{}{}}
	s.SetIdentRefresh(p.Refresh)
	return p, nil
}

// Refresh rewrites pg_ident.conf with the roles mapped now (the manager adds
// the map of the other servers); the caller reloads the instance.
func (p *Proxy) Refresh() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make([]string, 0, len(p.roles))
	for r := range p.roles {
		next = append(next, r)
	}
	return p.svc.mgr.WriteIdent(NodeID, p.osUser, next)
}

// Reset empties the map (kingresd start). The instance must be running or
// stopped; a stopped instance reads the file when it starts.
func (p *Proxy) Reset() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roles = map[string]struct{}{}
	return p.svc.mgr.WriteIdent(NodeID, p.osUser, nil)
}

// Allow maps the role; call it after a successful sign-in only.
func (p *Proxy) Allow(ctx context.Context, role string) error {
	if !p.svc.Enabled() || !p.svc.mgr.Exists(NodeID) {
		return errors.New("no managed instance")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.roles[role]; ok {
		return nil
	}
	next := make([]string, 0, len(p.roles)+1)
	for r := range p.roles {
		next = append(next, r)
	}
	next = append(next, role)
	sort.Strings(next)
	if err := p.svc.mgr.WriteIdent(NodeID, p.osUser, next); err != nil {
		return err
	}
	if err := p.svc.mgr.Reload(ctx, NodeID); err != nil {
		return err
	}
	p.roles[role] = struct{}{}
	return nil
}

// Revoke removes the mapping of a role (the role was dropped or disabled).
func (p *Proxy) Revoke(ctx context.Context, role string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.roles[role]; !ok {
		return
	}
	delete(p.roles, role)
	next := make([]string, 0, len(p.roles))
	for r := range p.roles {
		next = append(next, r)
	}
	if err := p.svc.mgr.WriteIdent(NodeID, p.osUser, next); err == nil {
		_ = p.svc.mgr.Reload(ctx, NodeID)
	}
}

// Connect opens a connection to database db as role. The role must have
// signed in (ErrNotEnabled otherwise). The caller closes the connection.
func (p *Proxy) Connect(ctx context.Context, db, role string) (*pgx.Conn, error) {
	p.mu.Lock()
	_, ok := p.roles[role]
	p.mu.Unlock()
	if !ok {
		return nil, ErrNotEnabled
	}
	sock, err := p.svc.mgr.SocketDir(NodeID)
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User = filepath.ToSlash(sock), uint16(p.svc.cfg.PGPort), db, role //nolint:gosec // port validated 1024..65535 by the configuration
	cfg.Password, cfg.TLSConfig, cfg.Fallbacks = "", nil, nil
	cfg.ConnectTimeout = 5 * time.Second
	// A statement of a signed-in role can select a value of any size; the driver would
	// read it into memory whole, so the stream is cut at the size of a result (CP-04).
	cfg.DialFunc = pgframe.Dialer(pgframe.MaxRoleMessage)
	cfg.RuntimeParams["application_name"] = "kingresd-proxy"
	// A reload that has just happened may not have reached a new backend yet.
	var last error
	for attempt := 0; attempt < 10; attempt++ {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err == nil {
			return conn, nil
		}
		last = err
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || len(pe.Code) < 2 || pe.Code[:2] != "28" {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, last
}
