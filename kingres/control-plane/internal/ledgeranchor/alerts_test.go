// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgeranchor

import (
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// 6.9: the absence of anchors is visible. A database with ledger tables and no enabled target has the alert ledgerNotAnchored (info; warning when
// anchor.required), it is cleared when a target is enabled, and a second pass with nothing changed writes nothing.
func TestTheAbsenceOfAnchorsIsAnAlertThatClearsWhenATargetIsEnabled(t *testing.T) {
	e := newEnv(t)
	if err := e.runMonitor(); err != nil {
		t.Fatal(err)
	}
	if e.alerts.severity(AlertNotAnchored, "database:shop") != "info" {
		t.Fatalf("alerts: %s", e.alerts.all())
	}
	w := e.alerts.writes
	_ = e.runMonitor()
	if e.alerts.writes != w {
		t.Error("a second pass with nothing changed wrote alerts")
	}
	e.st.SetSettings(Settings{Interval: time.Hour, Minimum: 1, Required: true})
	_ = e.runMonitor()
	if e.alerts.severity(AlertNotAnchored, "database:shop") != "warning" {
		t.Errorf("anchor.required: %s", e.alerts.all())
	}
	tg := e.addWorm("vault")
	_ = e.runMonitor()
	if e.alerts.severity(AlertNotAnchored, "database:shop") != "" {
		t.Errorf("not cleared: %s", e.alerts.all())
	}
	// a disabled target is not an anchor
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertNotAnchored, "database:shop") == "" {
		t.Error("a disabled target hid the alert")
	}
}

// ledgerAnchorBehind: warning after 3 intervals and an hour, critical after 12 and an hour, cleared by an anchor; and the state of the target says it.
func TestAnAnchorThatIsBehindIsWarnedThenCriticalAndClearedByAnAnchor(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	e.putAgg("shop", 1)
	_ = e.runEnqueue()
	e.s3.putStatus, e.s3.putCode = 503, "SlowDown" // the store is down
	warn, crit := BehindWarn(time.Hour), BehindCritical(time.Hour)
	e.clock.Advance(warn - time.Minute)
	_ = e.runMonitor()
	if e.alerts.severity(AlertBehind, "target:1") != "" {
		t.Fatalf("early: %s", e.alerts.all())
	}
	e.clock.Advance(2 * time.Minute)
	_ = e.runMonitor()
	if e.alerts.severity(AlertBehind, "target:1") != "warning" {
		t.Fatalf("warning: %s", e.alerts.all())
	}
	if got, _ := e.svc.Target(e.ctx(), tg.ID); got.State != "behind" {
		t.Errorf("state %s", got.State)
	}
	w := e.alerts.writes
	_ = e.runMonitor()
	if e.alerts.writes != w {
		t.Error("an unchanged alert was written again")
	}
	e.clock.Advance(crit - warn)
	_ = e.runMonitor()
	if e.alerts.severity(AlertBehind, "target:1") != "critical" {
		t.Fatalf("critical: %s", e.alerts.all())
	}
	// the store is back: the record goes through (after its backoff) and the alert is cleared
	e.s3.putStatus = 0
	e.clock.Advance(2 * time.Hour)
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertBehind, "target:1") != "" || e.rows()[0].State != anchor.StateAnchored {
		t.Errorf("after the anchor: %s %+v", e.alerts.all(), e.rows()[0])
	}
}

// ledgerAnchorFailed is raised for a failed record and cleared by a Retry; a deleted target takes its alerts with it; the text comes from the code of
// the error and holds no credential.
func TestAFailedRecordRaisesAnAlertThatARetryClears(t *testing.T) {
	e, id := oneRecord(t)
	e.s3.noLock = true
	_ = e.runSubmit()
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1") != "critical" {
		t.Fatalf("alerts: %s", e.alerts.all())
	}
	if !strings.Contains(e.alerts.all(), "not_locked") {
		t.Errorf("the text does not say why: %s", e.alerts.all())
	}
	noSecret(t, "the alerts", e.alerts.all(), testKeyID, testSecret)
	if got, _ := e.svc.Target(e.ctx(), 1); got.State != "failed" {
		t.Errorf("state %s", got.State)
	}
	e.s3.noLock = false
	if _, err := e.svc.Retry(e.ctx(), "shop", id); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1") != "" {
		t.Errorf("not cleared by the retry: %s", e.alerts.all())
	}
	// failing again and deleting the target
	e.s3.noLock = true
	e.s3.objects = map[string][]*fakeVersion{}
	_ = e.runSubmit()
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1") == "" {
		t.Fatalf("not raised again: %s", e.alerts.all())
	}
	if err := e.svc.DeleteTarget(e.ctx(), 1); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1") != "" {
		t.Error("the alert of a deleted target stays")
	}
	// the records stay and name the target
	pg, _ := e.svc.List(e.ctx(), "shop", 0, "", pageOf(10))
	if len(pg.Items) != 1 || pg.Items[0].TargetID != nil || pg.Items[0].TargetName != "vault" {
		t.Errorf("records after the delete: %+v", pg.Items)
	}
	if _, err := e.svc.Retry(e.ctx(), "shop", id); err == nil {
		t.Error("a record of a deleted target was retried")
	}
}

// REST rules of the targets.
func TestTargetsAreCreatedChangedAndDeletedByTheRules(t *testing.T) {
	e := newEnv(t)
	bad := func(name string, in api.LedgerAnchorTargetCreate, code problem.Code) {
		t.Helper()
		_, err := e.svc.CreateTarget(e.ctx(), "a", in)
		wantCode(t, err, code)
		_ = name
	}
	cfg := e.wormConfig(nil)
	bad("name", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "", Config: cfg}, problem.ValidationFailed)
	bad("name chars", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "a/b", Config: cfg}, problem.ValidationFailed)
	bad("kind", api.LedgerAnchorTargetCreate{Kind: "ethereum", Name: "x", Config: cfg}, problem.ValidationFailed)
	bad("config", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "x", Config: []byte(`{"bucket":"b"}`)}, problem.ValidationFailed)
	bad("config shape", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "x", Config: []byte(`[1]`)}, problem.ValidationFailed)
	bad("enabled without credentials", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "x", Enabled: true, Config: cfg}, problem.ValidationFailed)
	bad("both", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "x", Config: e.wormConfig(map[string]any{"credentialsPath": "/nonexistent"}), Credentials: wormCreds()}, problem.ValidationFailed)
	bad("path unreadable", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "x", Config: e.wormConfig(map[string]any{"credentialsPath": "/nonexistent"})}, problem.ValidationFailed)
	tg := e.addWorm("vault")
	_, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "vault", Config: cfg})
	wantCode(t, err, problem.AlreadyExists)
	if _, err = e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Kind: ptr("tsa")}); err == nil {
		t.Error("the kind changed")
	} else {
		wantCode(t, err, problem.ImmutableField)
	}
	_, err = e.svc.UpdateTarget(e.ctx(), 99, api.LedgerAnchorTargetUpdate{Name: ptr("z")})
	wantCode(t, err, problem.NotFound)
	if _, err = e.svc.Target(e.ctx(), 99); err == nil {
		t.Error("an unknown target")
	}
	// a rename, a config change that keeps the credentials, new credentials replace the old
	got, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Name: ptr("vault two"), Config: e.wormConfig(map[string]any{"retentionDays": 40})})
	if err != nil || got.Name != "vault two" || !got.CredentialsSet {
		t.Fatalf("update: %+v %v", got, err)
	}
	newCreds := mustRaw(map[string]string{"accessKeyId": "NEWKEYID1234", "secretAccessKey": "new/secret+value=9876"})
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: newCreds}); err != nil {
		t.Fatal(err)
	}
	c, err := e.creds.Get(tg.ID, "worm")
	if err != nil || c["accessKeyId"].Reveal() != "NEWKEYID1234" {
		t.Errorf("the credentials were not replaced: %v", err)
	}
	// the test now fails (the fake store knows only the old key): a failed check, not an error
	res, err := e.svc.TestTarget(e.ctx(), tg.ID)
	if err != nil || res.OK {
		t.Fatalf("test with wrong credentials: %+v %v", res, err)
	}
	noSecret(t, "the test", mustString(res), "new/secret+value=9876", "NEWKEYID1234")
	// removing the credentials of an enabled target is refused; of a disabled one it removes the file
	_, err = e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: []byte(`{}`)})
	wantCode(t, err, problem.ValidationFailed)
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Enabled: ptr(false), Credentials: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if e.creds.Has(tg.ID) {
		t.Error("the credentials file stays")
	}
	if err := e.svc.DeleteTarget(e.ctx(), tg.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteTarget(e.ctx(), tg.ID); err == nil {
		t.Error("deleted twice")
	}
	// the test of a good target passes every check and is stored
	good := e.addWorm("good")
	res, err = e.svc.TestTarget(e.ctx(), good.ID)
	if err != nil || !res.OK || res.TestedAt == nil {
		t.Fatalf("test: %+v %v", res, err)
	}
	if again, _ := e.svc.Target(e.ctx(), good.ID); again.LastTest == nil || !again.LastTest.OK {
		t.Errorf("the last test is not kept: %+v", again.LastTest)
	}
}

// credentialsPath: the operator's own file, read at every use, never copied, and refused when others can read it.
func TestCredentialsPathIsReadAtEveryUseAndNeverCopied(t *testing.T) {
	e := newEnv(t)
	p := writePassphraseWith(t, "x") // any owner-only file path
	body := `{"accessKeyId":"` + testKeyID + `","secretAccessKey":"` + testSecret + `"}`
	if err := writeFileMode(p, body); err != nil {
		t.Fatal(err)
	}
	tg, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "mounted", Enabled: true, Config: e.wormConfig(map[string]any{"credentialsPath": p})})
	if err != nil {
		t.Fatal(err)
	}
	if !tg.CredentialsSet || e.creds.Has(tg.ID) {
		t.Errorf("credentialsSet %v, a copy %v", tg.CredentialsSet, e.creds.Has(tg.ID))
	}
	if res, _ := e.svc.TestTarget(e.ctx(), tg.ID); !res.OK {
		t.Fatalf("test: %+v", res)
	}
	// the file changes: the next use reads the new content
	if err := writeFileMode(p, `{"accessKeyId":"OTHERKEY1234","secretAccessKey":"other-secret-value"}`); err != nil {
		t.Fatal(err)
	}
	if res, _ := e.svc.TestTarget(e.ctx(), tg.ID); res.OK {
		t.Error("the changed file was not read")
	}
}

// The scan of the design (10): a whole life of a sealed target, with a store that echoes the keys in its errors, is searched for every spelling of the
// key id and of the secret: the log, the alerts, the rows as printed and as JSON, the answers, the files of the key directory.
func TestNoCredentialAppearsAnywhereInTheLifeOfASealedTarget(t *testing.T) {
	e := newEnv(t)
	e.rebuild(writePassphrase(t))
	tg := e.addWorm("vault")
	e.putAgg("shop", 2)
	_ = e.runEnqueue()
	e.s3.putStatus, e.s3.putCode, e.s3.putMsg, e.s3.echoSecrets = 403, "AccessDenied", "denied", true
	for i := 0; i < 5; i++ {
		_ = e.runSubmit()
		_ = e.runMonitor()
		e.clock.Advance(20 * time.Minute)
	}
	res, _ := e.svc.TestTarget(e.ctx(), tg.ID)
	e.s3.putStatus, e.s3.echoSecrets = 0, false
	_, _ = e.svc.Retry(e.ctx(), "shop", e.rows()[0].ID)
	_ = e.runSubmit()
	_ = e.runMonitor()
	text := e.allText() + mustString(res)
	if f, err := e.svc.Evidence(e.ctx(), "shop", e.rows()[0].ID, "json"); err == nil {
		text += string(f.Data)
	}
	noSecret(t, "the life of a target", text, testKeyID, testSecret)
	for p, b := range e.keyFiles() {
		noSecret(t, "the file "+filepathBase(p), string(b), testKeyID, testSecret)
	}
	if e.row(e.rows()[0].ID).State != anchor.StateAnchored {
		t.Errorf("the record did not end anchored: %+v", e.rows()[0])
	}
}
