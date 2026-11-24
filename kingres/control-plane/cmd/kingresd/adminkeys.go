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
//
// What the file is. A StoredKey is SHA-256 of the client key: it checks a proof and cannot make one, and the same value is in pg_authid in the data directory of that
// instance, so the file alone adds nothing to what a reader of the data directory has (the instance's own files die with the instance; this one is removed when the
// server is reset or leaves). A proof that the old leader receives does teach a reader of the file the operator's client key, as for the proof of a map group failover
// (ADR 0026); the resume body is never logged.
//
// How current it is. The file is replaced after a change of the administrators or of a password, at most adminKeysEvery later. Until then a proof with the old password is
// refused, and an administrator who was just removed is still accepted. The file carries the cluster id and the time of the last successful read; a cache of another cluster is
// dropped, and one that is older than the bound (setting admin-keys-max-age, default 72 hours) is refused, so that a server that was down or held back for a long time does not
// accept a revoked administrator or an old password; the local command is the way out then. The other checks of the release stay: the hold must be for the declared
// server, which must be seen dead and silent, and the lease decides who starts.
const adminKeysEvery = 30 * time.Second

// adminKeysRewriteEvery is how often an unchanged cache is written again, so that the time in the file follows the last read within this much.
const adminKeysRewriteEvery = 10 * time.Minute

// adminKeysName is the file of the cache below the failover directory of the data directory (cluster.AdminKeysCacheFile is its path from the data directory).
const adminKeysName = "admin-keys.json"

const adminKeysSQL = `
WITH RECURSIVE admins(roleid) AS (
    SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'kingres_admin'
  UNION
    SELECT a.member FROM pg_catalog.pg_auth_members a JOIN admins ON a.roleid = admins.roleid WHERE a.inherit_option
)
SELECT r.rolname, x.rolpassword FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_authid x ON x.oid = r.oid
WHERE r.oid IN (SELECT roleid FROM admins) AND r.rolcanlogin AND x.rolpassword IS NOT NULL
  AND (x.rolvaliduntil IS NULL OR x.rolvaliduntil > now())`

// adminKeys is the cache; it implements fwdproof.KeySource over its contents only.
type adminKeys struct {
	path      string
	clusterID func() string
	maxAge    time.Duration
	now       func() time.Time

	mu          sync.Mutex
	keys        map[string][]byte
	loaded      bool
	cluster     string    // the cluster the contents are of
	refreshedAt time.Time // the last successful read
	writtenAt   time.Time // the last write of the file
}

type adminKeysDisk struct {
	Cluster     string            `json:"clusterId,omitempty"`
	RefreshedAt int64             `json:"refreshedAt"`
	Admins      map[string]string `json:"admins"`
}

// openAdminKeys reads the file of a former run, if there is one and it belongs to this cluster. clusterID tells the cluster of this server now ("" while it has none); maxAge
// is the bound on the age of the contents (0: no bound). The mode of an existing file is narrowed to 0600.
func openAdminKeys(dir string, clusterID func() string, maxAge time.Duration) *adminKeys {
	a := &adminKeys{path: filepath.Join(dir, adminKeysName), clusterID: clusterID, maxAge: maxAge, now: time.Now, keys: map[string][]byte{}}
	if a.clusterID == nil {
		a.clusterID = func() string { return "" }
	}
	b, err := os.ReadFile(a.path) //nolint:gosec // fixed name in the data directory
	if err != nil {
		return a
	}
	_ = os.Chmod(a.path, 0o600) // a copy by a restore or a chmod of an operator may have widened it
	var d adminKeysDisk
	if json.Unmarshal(b, &d) != nil {
		return a
	}
	if cur := a.clusterID(); cur != "" && d.Cluster != cur {
		// the file is of another cluster (the server was reset and joined another one)
		_ = os.Remove(a.path)
		return a
	}
	for u, h := range d.Admins {
		if k, err := hex.DecodeString(h); err == nil && len(k) == 32 {
			a.keys[u] = k
		}
	}
	a.loaded, a.cluster = true, d.Cluster
	a.refreshedAt = time.Unix(d.RefreshedAt, 0)
	a.writtenAt = a.refreshedAt
	return a
}

// StoredKey implements fwdproof.KeySource: a role that is not in the cache is not an administrator as far as this server knows (scram.ErrNotSCRAM, no proof is
// possible); a server that never cached anything, or whose cache is of another cluster or too old, cannot say.
func (a *adminKeys) StoredKey(_ context.Context, username string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loaded {
		return nil, errors.New("the keys of the administrators were never cached on this server")
	}
	if cur := a.clusterID(); cur != "" && a.cluster != "" && cur != a.cluster {
		return nil, errors.New("the keys of the administrators that were cached here are of another cluster")
	}
	if a.maxAge > 0 && a.now().Sub(a.refreshedAt) > a.maxAge {
		return nil, fmt.Errorf("the keys of the administrators were last read %s ago, more than admin-keys-max-age (%s)", a.now().Sub(a.refreshedAt).Round(time.Minute), a.maxAge)
	}
	k, ok := a.keys[username]
	if !ok {
		return nil, scram.ErrNotSCRAM
	}
	return append([]byte(nil), k...), nil
}

// Replace makes the administrators the given ones (role name to StoredKey), counts this as a successful read and writes the file when they changed or the file is old. A set
// that is empty does not replace one that is not: the administrators of a cluster are never none, and an empty answer is the answer of a wrong instance (phase 5 review F1).
func (a *adminKeys) Replace(next map[string][]byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(next) == 0 && len(a.keys) > 0 {
		return errors.New("the read found no administrator: the cache is kept")
	}
	now := a.now()
	same := a.loaded && len(next) == len(a.keys)
	if same {
		for u, k := range next {
			if !bytes.Equal(a.keys[u], k) {
				same = false
				break
			}
		}
	}
	cur := a.clusterID()
	if same && cur == a.cluster && now.Sub(a.writtenAt) < adminKeysRewriteEvery {
		a.refreshedAt = now
		return nil
	}
	d := adminKeysDisk{Cluster: cur, RefreshedAt: now.Unix(), Admins: map[string]string{}}
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
	a.keys, a.loaded, a.cluster, a.refreshedAt, a.writtenAt = next, true, cur, now, now
	return nil
}

// refresh reads the administrators with read and replaces the cache with them. A failed read leaves the cache as it is.
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

// serviceAdminVerifiers lists the verifiers of the administrators on an instance of the map group of this server: the primary of group 0, or else a standby of it (it holds
// the same roles). It never reads another instance: a shard primary has its own roles, and during the hold of the map group it is the one that still runs (phase 5 review F1).
func serviceAdminVerifiers(svc *cluster.Service) func(ctx context.Context) (map[string]string, error) {
	return func(ctx context.Context) (map[string]string, error) {
		id, ok := mapGroupInstance(svc)
		if !ok {
			return nil, errors.New("this server runs no instance of the map group")
		}
		c, err := svc.InstanceConn(ctx, id, "postgres")
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

// mapGroupInstance is the directory id of the instance of this server that belongs to group 0: its primary, or else a finished standby of it.
func mapGroupInstance(svc *cluster.Service) (int, bool) {
	return pickMapGroupInstance(svc.LocalInstances())
}

// pickMapGroupInstance chooses among the instances of this server: only group 0, a primary before a standby, never one whose copy is unfinished or that has no port.
func pickMapGroupInstance(all []cluster.LocalInstance) (int, bool) {
	standby := -1
	for _, li := range all {
		if li.Role.Group != 0 || li.Unfinished || li.Port == 0 {
			continue
		}
		if li.Role.Writable() {
			return li.ID, true
		}
		if li.Role.IsStandby() {
			standby = li.ID
		}
	}
	return standby, standby >= 0
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
