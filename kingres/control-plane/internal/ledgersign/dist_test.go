// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func TestAHolderThatCannotBeReachedDoesNotFailTheCreationAndIsGivenTheKeyLater(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2", "s3"))
	e.peers["s3"].down = true
	tk := e.create(0)
	fp := keyIn(t, load(t, e), ledgerkeys.StateActive).Fingerprint()
	if !e.peers["s2"].kr.Has(fp) || e.peers["s3"].kr.Has(fp) {
		t.Fatal("the key is on s2 and not on the unreachable s3")
	}
	if got := joined(load(t, e).Cluster.Holders); got != "s1,s2" {
		t.Errorf("the holders that have the key: %s", got)
	}
	if !strings.Contains(tk.Detail, "s3") {
		t.Errorf("the detail of the task does not name the server that lacks the key: %q", tk.Detail)
	}
	// The sync job sees the holder that lacks the key and starts the distribution; nothing happens while s3 is down.
	e.autorun = true
	n := len(e.started)
	if err := e.svc.Jobs()[2].Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(e.started) != n+1 || e.started[n].Kind != "ledger.key.distribute" {
		t.Fatalf("the sync job started %+v", e.started[n:])
	}
	if e.peers["s3"].kr.Has(fp) {
		t.Fatal("s3 has the key while it is down")
	}
	// A server that is down does not make the distribution fail: it is named, and tried again at the next pass.
	if got, _ := e.eng.Get(context.Background(), e.started[n].ID); got.State != task.Done || !strings.Contains(got.Detail, "s3") {
		t.Errorf("the distribution with s3 down: %s %+v %q", got.State, got.Error, got.Detail)
	}
	e.peers["s3"].down = false
	n = len(e.started)
	if err := e.svc.Jobs()[2].Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(e.started) != n+1 {
		t.Fatalf("the sync job started %d tasks after s3 came back", len(e.started)-n)
	}
	if !e.peers["s3"].kr.Has(fp) || joined(load(t, e).Cluster.Holders) != "s1,s2,s3" {
		t.Errorf("s3 has the key %v, holders %v", e.peers["s3"].kr.Has(fp), load(t, e).Cluster.Holders)
	}
	// Nothing left to do: the job starts no task.
	n = len(e.started)
	if err := e.svc.Jobs()[2].Run(context.Background(), 1); err != nil || len(e.started) != n {
		t.Errorf("the sync job with nothing to do: %v, started %d", err, len(e.started)-n)
	}
}

func TestAServerThatStopsRunningAnInstanceOfGroupZeroLosesTheKeyFiles(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2", "s3"))
	e.create(0)
	fp := keyIn(t, load(t, e), ledgerkeys.StateActive).Fingerprint()
	e.hold = []string{"s2"} // s3 no longer runs an instance of group 0
	e.autorun = true
	if err := e.svc.Jobs()[2].Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if e.peers["s3"].kr.Has(fp) || !e.peers["s2"].kr.Has(fp) || !e.kr.Has(fp) {
		t.Errorf("after the change of holders: s1 %v s2 %v s3 %v", e.kr.Has(fp), e.peers["s2"].kr.Has(fp), e.peers["s3"].kr.Has(fp))
	}
	if got := joined(load(t, e).Cluster.Holders); got != "s1,s2" {
		t.Errorf("holders %s", got)
	}
}

func TestARetirementThatCannotRemoveAFileFromAServerSaysSoAndTheDistributionRemovesItLater(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2"))
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(1)
	e.clk.Add(25 * time.Hour)
	e.peers["s2"].down = true
	tk, err := e.svc.RetireKey(context.Background(), "a", k1.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	got, _ := e.eng.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != "key_files_remain" || !got.Error.Retryable || !strings.Contains(got.Error.Message, "s2") {
		t.Fatalf("the retirement with a server down: %s %+v", got.State, got.Error)
	}
	// The key is retired in the record and its file is gone from this server; s2 still has one.
	if stateOf(load(t, e), k1.Fingerprint()) != ledgerkeys.StateRetired || e.kr.Has(k1.Fingerprint()) || !e.peers["s2"].kr.Has(k1.Fingerprint()) {
		t.Fatal("the state after the partial retirement")
	}
	e.peers["s2"].down = false
	e.autorun = true
	n := len(e.started)
	if err := e.svc.Jobs()[2].Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(e.started) != n+1 || e.peers["s2"].kr.Has(k1.Fingerprint()) {
		t.Errorf("the sync job did not remove the file of the retired key from s2 (started %d)", len(e.started)-n)
	}
}

func TestPassphrasesOfTheServersAgreeOrTheKeyIsNotInstalled(t *testing.T) {
	// The leader seals with its passphrase; a holder with another one cannot open the file and refuses it; a holder
	// without one refuses a sealed file; a holder that seals installs a plain file and keeps it sealed.
	e := newEnv(t, memFactory, withPassphrase("alpha alpha alpha"), withPeers("s2"))
	other, _ := ledgersign.NewKeyring(t.TempDir(), writePass(t, "beta beta beta"))
	none, _ := ledgersign.NewKeyring(t.TempDir(), "")
	t.Cleanup(other.Close)
	t.Cleanup(none.Close)
	e.peers["s3"] = &server{id: "s3", kr: other, agent: &ledgersign.Agent{Keyring: other}}
	e.peers["s4"] = &server{id: "s4", kr: none, agent: &ledgersign.Agent{Keyring: none}}
	e.hold = []string{"s2", "s3", "s4"}
	tk := e.create(0)
	fp := keyIn(t, load(t, e), ledgerkeys.StateActive).Fingerprint()
	if !e.peers["s2"].kr.Has(fp) || other.Has(fp) || none.Has(fp) {
		t.Errorf("s2 %v, s3 %v, s4 %v", e.peers["s2"].kr.Has(fp), other.Has(fp), none.Has(fp))
	}
	if !strings.Contains(tk.Detail, "s3") || !strings.Contains(tk.Detail, "s4") {
		t.Errorf("the detail does not name the servers that could not take the key: %q", tk.Detail)
	}
	// A refusal never prints a passphrase.
	e.noSecret("the detail", []byte(tk.Detail))
	e.noSecret("the log", []byte(e.logs.String()))
	// The agent says why: 409 passphrase_mismatch.
	data, err := e.kr.File(fp)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(ledgersign.InstallRequest{Fingerprint: fp, File: base64.StdEncoding.EncodeToString(data)})
	if _, err := e.peers["s3"].agent.Handle(context.Background(), ledgersign.CallInstall, body); err == nil || !strings.Contains(err.Error(), "passphrase_mismatch") {
		t.Errorf("a holder with another passphrase: %v", err)
	}
	// A plain file given to a sealing holder is sealed there.
	e2 := newEnv(t, memFactory, withPeers("s2"))
	e2.peers["s2"].kr.Close()
	sealing, _ := ledgersign.NewKeyring(t.TempDir(), writePass(t, "gamma gamma gamma"))
	t.Cleanup(sealing.Close)
	e2.peers["s2"].kr, e2.peers["s2"].agent = sealing, &ledgersign.Agent{Keyring: sealing}
	e2.create(0)
	fp2 := keyIn(t, load(t, e2), ledgerkeys.StateActive).Fingerprint()
	if prot, err := sealing.Protected(); err != nil || !prot || !sealing.Has(fp2) {
		t.Errorf("the holder that seals keeps the plain file it was given sealed: %v %v", prot, err)
	}
	if prot, _ := e2.kr.Protected(); prot {
		t.Error("the leader that does not seal reports its files as sealed")
	}
}

func TestTheAgentRefusesWhatIsNotAKeyAndRemovingIsIdempotent(t *testing.T) {
	e := newEnv(t, memFactory)
	a := e.svc.Agent()
	ctx := context.Background()
	for name, body := range map[string]string{
		"not json":          `{`,
		"no fingerprint":    `{"file":"AAAA"}`,
		"bad fingerprint":   `{"fingerprint":"../../etc/passwd","file":"AAAA"}`,
		"file not a key":    `{"fingerprint":"` + strings.Repeat("a", 64) + `","file":"AAAA"}`,
		"file not base64":   `{"fingerprint":"` + strings.Repeat("a", 64) + `","file":"***"}`,
		"fingerprint short": `{"fingerprint":"abc","file":"AAAA"}`,
	} {
		if _, err := a.Handle(ctx, ledgersign.CallInstall, []byte(body)); err == nil {
			t.Errorf("install %s was accepted", name)
		}
	}
	for _, body := range []string{`{"fingerprint":"` + strings.Repeat("b", 64) + `"}`} {
		if _, err := a.Handle(ctx, ledgersign.CallRemove, []byte(body)); err != nil {
			t.Errorf("removing a key that is not there: %v", err)
		}
	}
	if _, err := a.Handle(ctx, ledgersign.CallRemove, []byte(`{"fingerprint":"../x"}`)); err == nil {
		t.Error("a path was accepted as a fingerprint")
	}
	got, err := a.Handle(ctx, ledgersign.CallList, []byte(`{}`))
	if err != nil || len(got.(ledgersign.ListResponse).Fingerprints) != 0 {
		t.Errorf("list: %v %v", got, err)
	}
	if _, err := a.Handle(ctx, "POST /ledger/key/other", nil); err == nil {
		t.Error("an unknown call was answered")
	}
	// Installing the key a second time is a no-op; another key under the same name is refused.
	e.create(0)
	fp := keyIn(t, load(t, e), ledgerkeys.StateActive).Fingerprint()
	data, _ := e.kr.File(fp)
	req, _ := json.Marshal(ledgersign.InstallRequest{Fingerprint: fp, File: base64.StdEncoding.EncodeToString(data)})
	if _, err := a.Handle(ctx, ledgersign.CallInstall, req); err != nil {
		t.Errorf("installing a key that is there: %v", err)
	}
}

func writePass(t *testing.T, p string) string {
	t.Helper()
	f := tempDir(t) + "/pass"
	if err := os.WriteFile(f, []byte(p+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTheCheckJobRaisesAndClearsTheAlertOfTheSigner(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	fp := keyIn(t, load(t, e), ledgerkeys.StateActive).Fingerprint()
	ctx := context.Background()
	check := e.svc.Jobs()[1]
	e.alerts = nil
	if err := check.Run(ctx, 1); err != nil || len(e.alerts) != 1 || !strings.HasPrefix(e.alerts[0], "ledgerSignerUnavailable/clear/") {
		t.Fatalf("a healthy signer: %v %v", err, e.alerts)
	}
	// The key file of the active key is lost (a restored host without the key directory): the signer is unavailable.
	if err := e.kr.Remove(fp); err != nil {
		t.Fatal(err)
	}
	e.alerts = nil
	if err := check.Run(ctx, 1); err != nil || len(e.alerts) != 1 || !strings.HasPrefix(e.alerts[0], "ledgerSignerUnavailable/raise/") || !strings.Contains(e.alerts[0], "missing") {
		t.Fatalf("a missing key file: %v %v", err, e.alerts)
	}
	sig := e.signing()
	if sig.SignerAlarm == nil || !strings.Contains(*sig.SignerAlarm, "missing") {
		t.Errorf("getLedgerSigning: %+v", sig.SignerAlarm)
	}
	if _, _, err := e.svc.Signers(ctx); err == nil {
		t.Error("Signers with the file of the active key missing")
	}
}
