// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
)

// Defect 1: what the control plane records about a database lives in the leader's
// system database, so a follower's catalogs answered null where the leader had
// values. A follower completes those fields from the leader and keeps its own
// statistics.
func TestFollowerAnswersTheRecordedFieldsOfADatabase(t *testing.T) {
	fw := &direct{leader: "leader-1"}
	leader, follower := twoServers(t, fw)
	n, interval, mode := 32, 3600, "optIn"
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	by := "alice"
	own, size := int64(111), int64(222)
	leader.mem.DBs = []api.Database{{
		Name: "shop", Status: "ready",
		Sharding: api.ShardingSettings{Enabled: true, BucketCount: &n},
		Ledger:   api.LedgerSettings{Mode: &mode, DigestIntervalSeconds: &interval},
		Stats:    api.DatabaseStats{SizeBytes: &own}, CreatedBy: &by, CreatedAt: &at,
	}}
	off := "off"
	follower.mem.DBs = []api.Database{{
		Name: "shop", Status: "ready",
		Sharding: api.ShardingSettings{Enabled: true},
		Ledger:   api.LedgerSettings{Mode: &off},
		Stats:    api.DatabaseStats{SizeBytes: &size},
	}}
	leader.mem.LedgerByDB = map[string]api.LedgerStatus{"shop": {DatabaseName: "shop", Mode: &mode, DigestIntervalSeconds: &interval, Chains: []api.LedgerChainStatus{}}}
	follower.mem.LedgerByDB = map[string]api.LedgerStatus{"shop": {DatabaseName: "shop", Chains: []api.LedgerChainStatus{}}}

	check := func(name string, db api.Database) {
		t.Helper()
		if db.Sharding.BucketCount == nil || *db.Sharding.BucketCount != 32 {
			t.Errorf("%s: bucketCount = %v, want 32", name, db.Sharding.BucketCount)
		}
		if db.Ledger.Mode == nil || *db.Ledger.Mode != "optIn" || db.Ledger.DigestIntervalSeconds == nil || *db.Ledger.DigestIntervalSeconds != 3600 {
			t.Errorf("%s: ledger = %+v, want optIn / 3600", name, db.Ledger)
		}
		if db.CreatedBy == nil || *db.CreatedBy != "alice" || db.CreatedAt == nil {
			t.Errorf("%s: createdBy/createdAt = %v / %v", name, db.CreatedBy, db.CreatedAt)
		}
		if db.Stats.SizeBytes == nil || *db.Stats.SizeBytes != size {
			t.Errorf("%s: the follower's own statistics were replaced: %v", name, db.Stats.SizeBytes)
		}
	}

	w := follower.do(req{method: "GET", path: "/api/v1/databases/shop", bearer: follower.token(auth.RoleAdmin)})
	if w.Code != http.StatusOK {
		t.Fatalf("getDatabase: %d %s", w.Code, w.Body)
	}
	var db api.Database
	_ = json.Unmarshal(w.Body.Bytes(), &db)
	check("getDatabase", db)

	w = follower.do(req{method: "GET", path: "/api/v1/databases", bearer: follower.token(auth.RoleAdmin)})
	var list api.DatabaseList
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("listDatabases: %d %s", w.Code, w.Body)
	}
	check("listDatabases", list.Items[0])

	w = follower.do(req{method: "GET", path: "/api/v1/databases/shop/ledger", bearer: follower.token(auth.RoleAdmin)})
	var st api.LedgerStatus
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.Mode == nil || *st.Mode != "optIn" || st.DigestIntervalSeconds == nil || *st.DigestIntervalSeconds != 3600 {
		t.Errorf("getLedgerStatus: %d %s", w.Code, w.Body)
	}

	// The leader answers from its own store and asks nobody.
	calls := fw.calls
	w = leader.do(req{method: "GET", path: "/api/v1/databases/shop", bearer: leader.token(auth.RoleAdmin)})
	if w.Code != http.StatusOK || fw.calls != calls {
		t.Errorf("the leader asked for its own data: calls %d -> %d", calls, fw.calls)
	}
}
