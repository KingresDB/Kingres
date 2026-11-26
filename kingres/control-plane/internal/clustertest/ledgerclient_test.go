// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Step 17: client keys, the signature policy of a table and the upgrade of the extension, on a real cluster of two groups (a database of two
// shard groups; the ledger of each group has its own registry of keys).

func newClientKey(t *testing.T) (pub ed25519.PublicKey, b64, short string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pub)
	return pub, base64.StdEncoding.EncodeToString(pub), hex.EncodeToString(sum[:])[:8]
}

// sqlCount runs a query that returns one number on the group of the node.
func sqlCount(t *testing.T, n *clustertest.Node, db, q string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	c := n.ServicePG(db)
	defer func() { _ = c.Close(ctx) }()
	var v int
	if err := c.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func sqlText(t *testing.T, n *clustertest.Node, db, q string) string {
	t.Helper()
	ctx := context.Background()
	c := n.ServicePG(db)
	defer func() { _ = c.Close(ctx) }()
	var v string
	if err := c.QueryRow(ctx, q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestLedgerClientKeysAndPolicyOnTwoGroups(t *testing.T) {
	p := formPublishing(t, "ck_db")
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ledgerTable(t, leader, "ck_db", "t0", 2)
	ledgerTable(t, follower, "ck_db", "t1", 2)
	ctx := context.Background()
	if c := leader.ServicePG("ck_db"); true {
		_, err := c.Exec(ctx, "CREATE TABLE plain_t (id int PRIMARY KEY)")
		_ = c.Close(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	base := "/databases/ck_db/ledger/client-keys"

	// nothing is registered yet
	if b := leader.Expect(200, "GET", base, nil); len(anyList(b["items"])) != 0 {
		t.Fatalf("keys of a new ledger: %v", b)
	}

	// registered through the follower, so that the call reaches the leader: the key is in the registry of both groups
	pub, b64, short := newClientKey(t)
	k := follower.Expect(201, "POST", base, map[string]any{"publicKey": b64, "label": "the application"})
	if k["keyId"] != short || k["publicKey"] != b64 || k["label"] != "the application" || k["roleName"] != nil || k["revokedAt"] != nil {
		t.Fatalf("the registered key: %v", k)
	}
	if by, _ := k["registeredBy"].(string); by != clustertest.AdminUser {
		t.Errorf("registeredBy %q, want the signed-in role %q", by, clustertest.AdminUser)
	}
	for _, n := range []*clustertest.Node{leader, follower} {
		if got := sqlCount(t, n, "ck_db", "SELECT count(*)::int FROM kingres.ledger_client_key WHERE public_key = $1", []byte(pub)); got != 1 {
			t.Errorf("node %d: the key is in %d rows of its registry", n.Index, got)
		}
		if l := n.Expect(200, "GET", base, nil); len(anyList(l["items"])) != 1 || asMap(anyList(l["items"])[0])["keyId"] != short {
			t.Errorf("node %d lists %v", n.Index, l)
		}
	}

	// refusals: the same key, bad input, a role that does not exist, a key of small order (the identity point), a role that may only read
	leader.Expect(409, "POST", base, map[string]any{"publicKey": b64, "label": "again"})
	for _, bad := range []map[string]any{
		{"publicKey": "not base64!", "label": "x"},
		{"publicKey": base64.StdEncoding.EncodeToString([]byte("short")), "label": "x"},
		{"publicKey": b64, "label": ""},
		{"publicKey": newKeyB64(t), "label": strings.Repeat("x", 101)},
		{"publicKey": newKeyB64(t), "label": "x", "roleName": "no_such_role"},
		{"publicKey": base64.StdEncoding.EncodeToString(append([]byte{1}, make([]byte, 31)...)), "label": "identity point"},
		{"publicKey": newKeyB64(t), "label": "x", "unknownMember": 1},
	} {
		if st, b := leader.Do("POST", base, bad, nil); st != 400 {
			t.Errorf("%v: %d %v", bad, st, b)
		}
	}
	for _, n := range []*clustertest.Node{leader, follower} {
		if got := sqlCount(t, n, "ck_db", "SELECT count(*)::int FROM kingres.ledger_client_key"); got != 1 {
			t.Errorf("node %d: %d keys after the refusals", n.Index, got)
		}
	}
	// a role bound key
	_, bound, boundShort := newClientKey(t)
	bk := leader.Expect(201, "POST", base, map[string]any{"publicKey": bound, "label": "bound", "roleName": clustertest.AdminUser})
	if bk["roleName"] != clustertest.AdminUser || bk["keyId"] != boundShort {
		t.Errorf("the bound key: %v", bk)
	}

	// revocation: in both groups; a second revocation and an unknown key are refused
	rv := leader.Expect(200, "POST", base+"/"+short+"/revoke", map[string]any{"reason": "the laptop was lost"})
	if rv["revokedAt"] == nil || rv["revocationReason"] != "the laptop was lost" {
		t.Fatalf("the revoked key: %v", rv)
	}
	for _, n := range []*clustertest.Node{leader, follower} {
		if got := sqlCount(t, n, "ck_db", "SELECT count(*)::int FROM kingres.ledger_client_key_revocation"); got != 1 {
			t.Errorf("node %d: %d revocations", n.Index, got)
		}
	}
	follower.Expect(409, "POST", base+"/"+short+"/revoke", map[string]any{"reason": "twice"})
	leader.Expect(404, "POST", base+"/ffffffff/revoke", map[string]any{"reason": "unknown"})
	leader.Expect(400, "POST", base+"/"+boundShort+"/revoke", map[string]any{"reason": ""})
	if l := follower.Expect(200, "GET", base, nil); len(anyList(l["items"])) != 2 {
		t.Errorf("the list after a revocation: %v", l)
	}

	// the signature policy: required for good, on the group that holds the table
	pol := "/databases/ck_db/schemas/public/tables/t0/ledger/signature-policy"
	tb := follower.Expect(200, "PUT", pol, map[string]any{"policy": "required"})
	if tb["name"] != "t0" || tb["ledger"] == nil {
		t.Errorf("the table: %v", tb)
	}
	leader.Expect(409, "PUT", pol, map[string]any{"policy": "required"})
	leader.Expect(400, "PUT", pol, map[string]any{"policy": "optional"})
	leader.Expect(422, "PUT", "/databases/ck_db/schemas/public/tables/plain_t/ledger/signature-policy", map[string]any{"policy": "required"})
	leader.Expect(404, "PUT", "/databases/ck_db/schemas/public/tables/no_such/ledger/signature-policy", map[string]any{"policy": "required"})
	if got := sqlText(t, leader, "ck_db", "SELECT signature_policy FROM kingres.ledger_table WHERE relid = 'public.t0'::regclass"); got != "required" {
		t.Errorf("policy of t0 in the registry: %s", got)
	}
	// an entry without an attestation is refused at commit by the database itself
	if c := leader.ServicePG("ck_db"); true {
		_, err := c.Exec(ctx, "INSERT INTO t0 VALUES (100, 'unsigned')")
		_ = c.Close(ctx)
		if err == nil || !strings.Contains(err.Error(), "KL010") {
			t.Errorf("an unattested row in a required table: %v", err)
		}
	}

	// a reader may list the keys and nothing else
	mkRole(t, leader, "reader_ck", markerPassword, "kingres_readonly")
	adminToken := leader.Token
	if !leader.Login("reader_ck", markerPassword) {
		t.Fatal("the reader cannot sign in")
	}
	if st, _ := leader.Do("GET", base, nil, nil); st != 200 {
		t.Errorf("a reader lists the keys: %d", st)
	}
	if st, _ := leader.Do("POST", base, map[string]any{"publicKey": newKeyB64(t), "label": "x"}, nil); st != 403 {
		t.Errorf("a reader registers a key: %d", st)
	}
	leader.Token = adminToken
}

func newKeyB64(t *testing.T) string {
	_, b, _ := newClientKey(t)
	return b
}

// With the extension at 0.2 the five operations answer as the contract says (422 unsupported_operation, or the policy on a table that has the
// ledger), and the task ledger.upgrade brings the database to 0.3 in both groups; a group that is still on 0.2 is named by the refusal.
func TestLedgerUpgradeAcrossTwoGroupsAndTheKeysThatNeedIt(t *testing.T) {
	p := formPublishing(t)
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "old_db", "sharding": map[string]any{"enabled": true, "bucketCount": 32}, "ledger": map[string]any{"mode": "off"}})
	leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	ctx := context.Background()
	// the group of the follower is on 0.2, the group of the leader is on the version of the library: the mixed state of a half made upgrade
	for _, n := range []*clustertest.Node{leader, follower} {
		v := "0.2"
		if n == leader {
			v = ""
		}
		c := n.ServicePG("old_db")
		q := "CREATE EXTENSION kingres_ledger"
		if v != "" {
			q += " VERSION '" + v + "'"
		}
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		_ = c.Close(ctx)
	}
	if got := sqlText(t, follower, "old_db", "SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'"); got != "0.2" {
		t.Fatalf("the follower's extension is at %s", got)
	}
	base := "/databases/old_db/ledger/client-keys"
	_, b64, _ := newClientKey(t)
	// the group of the follower is named, and nothing was registered on the leader's group
	st, e := leader.Do("POST", base, map[string]any{"publicKey": b64, "label": "x"}, nil)
	if st != 422 || e["code"] != "unsupported_operation" || !strings.Contains(fmt.Sprint(e["detail"]), "server") {
		t.Fatalf("a registration with a group on 0.2: %d %v", st, e)
	}
	if got := sqlCount(t, leader, "old_db", "SELECT count(*)::int FROM kingres.ledger_client_key"); got != 0 {
		t.Errorf("%d keys on the group that is on 0.3 after the refusal", got)
	}
	// a database whose both groups are on 0.2 answers 422 to the list
	if st, e := follower.Do("GET", base, nil, nil); st != 422 || e["code"] != "unsupported_operation" {
		t.Errorf("the list on a group of 0.2: %d %v", st, e)
	}
	// a ledger table on the group that is on 0.3: the group on 0.2 refuses the policy, and nothing changes on the first
	ledgerTable(t, leader, "old_db", "lt", 1)
	if st, e := leader.Do("PUT", "/databases/old_db/schemas/public/tables/lt/ledger/signature-policy", map[string]any{"policy": "required"}, nil); st != 422 || e["code"] != "unsupported_operation" {
		t.Errorf("the policy with a group on 0.2: %d %v", st, e)
	}
	if got := sqlText(t, leader, "old_db", "SELECT signature_policy FROM kingres.ledger_table WHERE relid = 'public.lt'::regclass"); got != "optional" {
		t.Errorf("the policy of lt after the refusal: %s", got)
	}

	// the upgrade: one task, both groups
	task := leader.Expect(202, "POST", "/databases/old_db/ledger/upgrade", map[string]any{})
	if task["kind"] != "ledger.upgrade" {
		t.Fatalf("the task: %v", task)
	}
	done := leader.WaitTaskDone(clustertest.TaskID(task), 4*time.Minute)
	if done["state"] != "done" {
		t.Fatalf("the upgrade: %v", done)
	}
	for _, n := range []*clustertest.Node{leader, follower} {
		if got := sqlText(t, n, "old_db", "SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'"); got != "0.3" {
			t.Errorf("node %d is at %s after the upgrade", n.Index, got)
		}
	}
	// a second upgrade has nothing to do and succeeds
	again := follower.Expect(202, "POST", "/databases/old_db/ledger/upgrade", nil)
	if d := leader.WaitTaskDone(clustertest.TaskID(again), 4*time.Minute); d["state"] != "done" {
		t.Errorf("the second upgrade: %v", d)
	}
	// and now the operations work
	leader.Expect(201, "POST", base, map[string]any{"publicKey": b64, "label": "after the upgrade"})
	for _, n := range []*clustertest.Node{leader, follower} {
		if l := n.Expect(200, "GET", base, nil); len(anyList(l["items"])) != 1 {
			t.Errorf("node %d lists %v", n.Index, l)
		}
	}
	// a database without the ledger: the upgrade says so
	mkDB(t, leader, "no_ledger_db")
	if st, e := leader.Do("POST", "/databases/no_ledger_db/ledger/upgrade", nil, nil); st != 409 {
		t.Errorf("the upgrade of a database without the ledger: %d %v", st, e)
	}
	leader.Expect(404, "POST", "/databases/no_such_db/ledger/upgrade", nil)
}

// Correction 211: a shard group whose chain is published for the first time gets the keys of the registry of the leader's group, revocations
// included. The keys are put into the leader's group alone with SQL, as they are when a group is added after they were registered.
func TestANewChainGetsTheClientKeysOfTheLeadersRegistry(t *testing.T) {
	p := formPublishing(t, "sync_db")
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ledgerTable(t, leader, "sync_db", "t0", 2)
	ledgerTable(t, follower, "sync_db", "t1", 2)
	ctx := context.Background()
	pubA, _, shortA := newClientKey(t)
	pubB, _, _ := newClientKey(t)
	c := leader.ServicePG("sync_db")
	var idB []byte
	for i, pk := range [][]byte{pubA, pubB} {
		var id []byte
		if err := c.QueryRow(ctx, "SELECT kingres.ledger_client_key_register($1, NULL, $2)", pk, fmt.Sprintf("key %d", i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		idB = id
	}
	if _, err := c.Exec(ctx, "SELECT kingres.ledger_client_key_revoke($1, 'lost')", idB); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(ctx)
	if got := sqlCount(t, follower, "sync_db", "SELECT count(*)::int FROM kingres.ledger_client_key"); got != 0 {
		t.Fatalf("the follower's group has %d keys before the publication", got)
	}
	p.startPublishing(t)
	until(t, 4*time.Minute, "the keys to reach the group of the follower", func() bool {
		return sqlCount(t, follower, "sync_db", "SELECT count(*)::int FROM kingres.ledger_client_key") == 2 &&
			sqlCount(t, follower, "sync_db", "SELECT count(*)::int FROM kingres.ledger_client_key_revocation") == 1
	})
	l := follower.Expect(200, "GET", "/databases/sync_db/ledger/client-keys", nil)
	if len(anyList(l["items"])) != 2 || asMap(anyList(l["items"])[0])["keyId"] != shortA {
		t.Errorf("the list on the follower: %v", l)
	}
}
