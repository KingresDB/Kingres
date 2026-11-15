// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"sort"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// Server groups and what is recorded about a server beyond the shard catalog
// live in the system database (kingres_cp.server_group, kingres_cp.server),
// which only the manager leader holds. Where that schema does not exist (a
// follower, a catalog without the control plane) these functions change
// nothing and the shard catalog's own server_group column is what is shown;
// kingresd never writes that column, so it is empty on a cluster it manages.
// The HTTP layer completes the answer of a follower from the leader.

// serverRecord is a row of kingres_cp.server, reduced to what the API shows.
type serverRecord struct {
	group   *string
	seen    string
	pinned  bool
	certPEM string
	revoked *time.Time
	joined  time.Time
}

// loadRecords reads kingres_cp.server; ok is false when the schema is absent.
func (s *Store) loadRecords(ctx context.Context) (rec map[string]serverRecord, ok bool, err error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return nil, false, err
	}
	if ok, err = s.regclassExists(ctx, p, "kingres_cp.server"); err != nil || !ok {
		return nil, false, err
	}
	rows, err := p.Query(ctx, `SELECT server_id, server_group, coalesce(seen_fingerprint, ''), fingerprint_pinned, coalesce(certificate_pem, ''), certificate_revoked_at, joined_at
		FROM kingres_cp.server`)
	if err != nil {
		return nil, false, s.wrap("reading kingres_cp.server", err)
	}
	defer rows.Close()
	rec = map[string]serverRecord{}
	for rows.Next() {
		var id string
		var r serverRecord
		if err := rows.Scan(&id, &r.group, &r.seen, &r.pinned, &r.certPEM, &r.revoked, &r.joined); err != nil {
			return nil, false, s.wrap("scanning kingres_cp.server", err)
		}
		rec[id] = r
	}
	if err := rows.Err(); err != nil {
		return nil, false, s.wrap("reading kingres_cp.server", err)
	}
	return rec, true, nil
}

// applyRecords sets the group, the fingerprint and the certificate expiry of
// the servers from the system database. A recorded group replaces the one of
// the catalog (also "no group"): the system database is the source of truth.
func (s *Store) applyRecords(ctx context.Context, servers []api.Server) {
	rec, ok, err := s.loadRecords(ctx)
	if err != nil {
		s.opts.Logger.Debug("reading the server records failed; the shard catalog's values are shown", "error", err)
		return
	}
	if !ok {
		return
	}
	for i := range servers {
		r, found := rec[servers[i].ID]
		if !found {
			continue
		}
		servers[i].ServerGroup = r.group
		if fp := pki.NormalizeFingerprint(r.seen); fp != "" {
			servers[i].SeenFingerprint = &fp
			servers[i].FingerprintPinned = r.pinned
		}
		if !r.joined.IsZero() {
			t := r.joined.UTC()
			servers[i].JoinedAt = &t
		}
		if r.revoked != nil {
			t := r.revoked.UTC()
			servers[i].CertificateRevokedAt = &t
		}
		if r.certPEM != "" {
			if c, err := pki.ParseCertPEM([]byte(r.certPEM)); err == nil {
				t := c.NotAfter.UTC()
				servers[i].CertificateExpiresAt = &t
			}
		}
	}
}

// recordedGroups reads the groups of the system database with their members.
func (s *Store) recordedGroups(ctx context.Context) ([]api.ServerGroup, bool, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return nil, false, err
	}
	ok, err := s.regclassExists(ctx, p, "kingres_cp.server_group")
	if err != nil || !ok {
		return nil, false, err
	}
	rows, err := p.Query(ctx, `SELECT name, description FROM kingres_cp.server_group`)
	if err != nil {
		return nil, false, s.wrap("reading kingres_cp.server_group", err)
	}
	byName := map[string]*api.ServerGroup{}
	for rows.Next() {
		g := api.ServerGroup{ServerIDs: []string{}}
		if err := rows.Scan(&g.Name, &g.Description); err != nil {
			rows.Close()
			return nil, false, s.wrap("scanning kingres_cp.server_group", err)
		}
		byName[g.Name] = &g
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, s.wrap("reading kingres_cp.server_group", err)
	}
	members, err := p.Query(ctx, `SELECT server_group, server_id FROM kingres_cp.server WHERE server_group IS NOT NULL`)
	if err != nil {
		return nil, false, s.wrap("reading kingres_cp.server", err)
	}
	defer members.Close()
	for members.Next() {
		var g, id string
		if err := members.Scan(&g, &id); err != nil {
			return nil, false, s.wrap("scanning kingres_cp.server", err)
		}
		if gr := byName[g]; gr != nil {
			gr.ServerIDs = append(gr.ServerIDs, id)
		}
	}
	if err := members.Err(); err != nil {
		return nil, false, s.wrap("reading kingres_cp.server", err)
	}
	out := make([]api.ServerGroup, 0, len(byName))
	for _, g := range byName {
		sort.Strings(g.ServerIDs)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, true, nil
}
