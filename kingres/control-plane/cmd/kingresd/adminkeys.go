// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

// The declaration that a server is lost (abandonMapGroupBootstrap with lostServerId) ends the hold of the old leader of the map group, whose instance (the system
// database) is stopped while it is held back, so the old leader cannot read a role's verifier to check the operator's proof (phase 5 review H1). It keeps a cache on disk
// instead: while it runs, the server that has the system database copies the StoredKeys of the roles that hold kingres_admin into a file of its data directory.
// A StoredKey is SHA-256 of the client key: it checks a proof and cannot make one, and the same value is in pg_authid in the data directory of that instance,
// so the file adds nothing to what a reader of the data directory has. The file is replaced after a change of the administrators or of a password, at most
// adminKeysEvery later; until then a proof with the old password is refused, and an administrator who was just removed is still accepted (the other checks
// of the release stay: the hold must be for the declared server, which must be silent, and the lease decides who starts).
const adminKeysEvery = 30 * time.Second

// adminKeysFile is the file of the cache below the failover directory of the data directory.
const adminKeysFile = "admin-keys.json"

const adminKeysSQL = `
WITH RECURSIVE admins(roleid) AS (
    SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'kingres_admin'
  UNION
    SELECT a.member FROM pg_catalog.pg_auth_members a JOIN admins ON a.roleid = admins.roleid WHERE a.inherit_option
)
SELECT r.rolname, x.rolpassword FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_authid x ON x.oid = r.oid
WHERE r.oid IN (SELECT roleid FROM admins) AND r.rolcanlogin AND x.rolpassword IS NOT NULL`

// adminKeys is the cache; it implements fwdproof.KeySource over its contents only.
type adminKeys struct {
	path string

	mu     sync.Mutex
	keys   map[string][]byte
	loaded bool
}

type adminKeysDisk struct {
	Admins map[string]string `json:"admins"`
}

// openAdminKeys reads the file of a former run, if there is one.
func openAdminKeys(dir string) *adminKeys {
	a := &adminKeys{path: filepath.Join(dir, adminKeysFile), keys: map[string][]byte{}}
	b, err := os.ReadFile(a.path) //nolint:gosec // fixed name in the data directory
	if err != nil {
		return a
	}
	var d adminKeysDisk
	if json.Unmarshal(b, &d) != nil {
		return a
	}
	for u, h := range d.Admins {
		if k, err := hex.DecodeString(h); err == nil && len(k) == 32 {
			a.keys[u] = k
		}
	}
	a.loaded = true
	return a
}

// StoredKey implements fwdproof.KeySource: a role that is not in the cache is not an administrator as far as this server knows (scram.ErrNotSCRAM, no proof is
// possible); a server that never cached anything cannot say.
func (a *adminKeys) StoredKey(_ context.Context, username string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loaded {
		return nil, errors.New("the keys of the administrators were never cached on this server")
	}
	k, ok := a.keys[username]
	if !ok {
		return nil, scram.ErrNotSCRAM
	}
	return append([]byte(nil), k...), nil
}

// Replace makes the administrators the given ones (role name to StoredKey) and writes the file when they changed.
func (a *adminKeys) Replace(next map[string][]byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	same := a.loaded && len(next) == len(a.keys)
	if same {
		for u, k := range next {
			if !bytes.Equal(a.keys[u], k) {
				same = false
				break
			}
		}
	}
	if same {
		return nil
	}
	d := adminKeysDisk{Admins: map[string]string{}}
	for u, k := range next {
		d.Admins[u] = hex.EncodeToString(k)
	}
	b, _ := json.Marshal(d)
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	if err := pki.WriteFile(a.path, b, 0o600); err != nil {
		return err
	}
	a.keys, a.loaded = next, true
	return nil
}

// refresh reads the administrators with read and replaces the cache with them.
func (a *adminKeys) refresh(ctx context.Context, read func(ctx context.Context) (map[string]string, error)) error {
	verifiers, err := read(ctx)
	if err != nil {
		return err
	}
	next := map[string][]byte{}
	for u, v := range verifiers {
		if p, err := scram.Parse(v); err == nil && len(p.StoredKey) == 32 {
			next[u] = p.StoredKey
		}
	}
	return a.Replace(next)
}

// serviceAdminVerifiers lists the verifiers of the administrators on the instance of this server that holds the system database (a standby serves the read as well).
func serviceAdminVerifiers(svc *cluster.Service) func(ctx context.Context) (map[string]string, error) {
	return func(ctx context.Context) (map[string]string, error) {
		c, err := svc.InstanceConn(ctx, svc.ConnectID(), "postgres")
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		rows, err := c.Query(ctx, adminKeysSQL, pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, verifier string
			if err := rows.Scan(&name, &verifier); err != nil {
				return nil, err
			}
			out[name] = verifier
		}
		return out, rows.Err()
	}
}

// runAdminKeys keeps the cache current until ctx ends. A failed read (the instance is down, or this server holds no copy of the system database) leaves the cache as it is.
func runAdminKeys(ctx context.Context, a *adminKeys, read func(ctx context.Context) (map[string]string, error), every time.Duration, log *slog.Logger) {
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := a.refresh(rctx, read); err != nil {
			log.Debug("the keys of the administrators were not cached", "error", fmt.Sprint(err))
		}
		cancel()
		t.Reset(every)
	}
}
