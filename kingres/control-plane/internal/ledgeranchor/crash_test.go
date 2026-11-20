// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

var errKilled = errors.New("killed")

// A kill at each point of a submit leaves a row that the next pass completes: the start of the first submission is durable before the call and never
// changes, the store holds one version of the note, and the record ends anchored. The points are the writes of the job (the hook of the store) and the
// steps between them.
func TestAKillAtEachPointOfASubmitIsCompletedByTheNextPass(t *testing.T) {
	points := []string{"after-begin", "after-call", "before-apply"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			e, id := oneRecord(t)
			fired := false
			e.svc.cfg.Hook = func(p string) error {
				if p == point && !fired {
					fired = true
					return errKilled
				}
				return nil
			}
			if err := e.runSubmit(); err != nil && !errors.Is(err, errKilled) {
				t.Fatal(err)
			}
			if !fired {
				t.Fatal("the point was not reached")
			}
			r := e.row(id)
			if r.State != anchor.StatePending || r.SubmitStart.IsZero() {
				t.Fatalf("after the kill: %+v", r)
			}
			start := r.SubmitStart
			// a restart: a new service over the same store
			e.rebuildKeepingCreds()
			e.clock.Advance(time.Minute)
			if err := e.runSubmit(); err != nil {
				t.Fatal(err)
			}
			r = e.row(id)
			if r.State != anchor.StateAnchored || !r.SubmitStart.Equal(start) {
				t.Fatalf("after the restart: %+v (start was %v)", r, start)
			}
			ev, err := anchor.ParseWORMEvidence(r.Evidence)
			if err != nil || len(e.s3.stored(ev.Key)) != 1 {
				t.Fatalf("evidence %v, %d versions", err, len(e.s3.stored(ev.Key)))
			}
		})
	}
}

// rebuildKeepingCreds is a restart: the service is made again with the same credentials directory and store.
func (e *env) rebuildKeepingCreds() { e.rebuild("") }

// A kill at each write of the store (the hook of the store makes the write fail before it changes anything) leaves nothing half done.
func TestAWriteThatFailsChangesNothingAndTheNextPassDoesIt(t *testing.T) {
	for _, kind := range []string{"enqueue", "begin", "apply"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			e.addWorm("vault")
			e.putAgg("shop", 2)
			fail := true
			e.st.Hook = func(k string) error {
				if k == kind && fail {
					return errKilled
				}
				return nil
			}
			_ = e.runEnqueue()
			if kind != "enqueue" {
				e.st.Hook = nil
				if err := e.runEnqueue(); err != nil {
					t.Fatal(err)
				}
				e.st.Hook = func(k string) error {
					if k == kind && fail {
						return errKilled
					}
					return nil
				}
			}
			_ = e.runSubmit()
			if kind == "enqueue" && len(e.rows()) != 0 {
				t.Fatal("a failed write left a record")
			}
			fail = false
			if err := e.runEnqueue(); err != nil {
				t.Fatal(err)
			}
			if err := e.runSubmit(); err != nil {
				t.Fatal(err)
			}
			rs := e.rows()
			if len(rs) != 1 || rs[0].State != anchor.StateAnchored {
				t.Fatalf("%+v", rs)
			}
			if len(e.s3.objects) != 1 {
				t.Errorf("%d keys", len(e.s3.objects))
			}
		})
	}
}

// The rule for outgoing requests: only the host and port of the configuration; an address that is written is connected to as written; a name that
// resolves to a loopback, link-local or unspecified address is refused; a private address is fine for a name.
func TestOutgoingRequestsGoOnlyWhereTheConfigurationSays(t *testing.T) {
	eg, err := NewEgress([]string{"https://store.example:9000", "http://127.0.0.1:9001"})
	if err != nil {
		t.Fatal(err)
	}
	dialed := ""
	eg.Dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		return nil, errors.New("not dialed")
	}
	eg.Resolve = func(_ context.Context, h string) ([]netip.Addr, error) {
		switch h {
		case "store.example":
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		case "meta.example":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	try := func(addr string) error { dialed = ""; _, err := eg.DialContext(bg(), "tcp", addr); return err }
	// not named
	if err := try("evil.example:9000"); !anchor.IsPermanent(err) || anchor.CodeOf(err) != "egress_refused" || dialed != "" {
		t.Errorf("a host that is not named: %v (dialed %q)", err, dialed)
	}
	if err := try("store.example:9999"); anchor.CodeOf(err) != "egress_refused" {
		t.Errorf("another port: %v", err)
	}
	// named, private: allowed, and dialed at the address that was checked
	if _ = try("store.example:9000"); dialed != "10.1.2.3:9000" {
		t.Errorf("dialed %q", dialed)
	}
	// a name that leads to the metadata address or to this machine
	for _, h := range []string{"meta.example", "loop.example"} {
		eg2, _ := NewEgress([]string{"https://" + h + ":443"})
		eg2.Resolve, eg2.Dial = eg.Resolve, eg.Dial
		dialed = ""
		_, err := eg2.DialContext(bg(), "tcp", h+":443")
		if anchor.CodeOf(err) != "egress_address" || dialed != "" {
			t.Errorf("%s: %v (dialed %q)", h, err, dialed)
		}
	}
	// an address that is written is the operator's choice, loopback included
	if _ = try("127.0.0.1:9001"); dialed != "127.0.0.1:9001" {
		t.Errorf("a written loopback address: dialed %q", dialed)
	}
}

// A credentials file in clear and sealed: the sealed one holds no spelling of a credential, has the mode 0600, and a wrong passphrase or a damaged file
// does not open; the rules for the members.
func TestCredentialsFilesAreSealedAndNeverReturned(t *testing.T) {
	e := newEnv(t)
	pass := writePassphrase(t)
	e.rebuild(pass)
	tg := e.addWorm("vault")
	for p, b := range e.keyFiles() {
		if filepathBase(p) == "1.json" {
			noSecret(t, "the sealed credentials file", string(b), testKeyID, testSecret)
			checkMode(t, p)
		}
	}
	if !e.creds.Has(tg.ID) {
		t.Fatal("no credentials file")
	}
	got, err := e.creds.Get(tg.ID, "worm")
	if err != nil || got["accessKeyId"].Reveal() != testKeyID || got["secretAccessKey"].Reveal() != testSecret {
		t.Fatalf("read back: %v", err)
	}
	// another passphrase
	other := writePassphraseWith(t, "another passphrase of this server")
	cs, _ := NewCredStore(e.dir, other)
	cs.MinIterations = 1000
	if _, err := cs.Get(tg.ID, "worm"); err == nil {
		t.Error("a file was opened with another passphrase")
	}
	// the API never returns them
	l, _ := e.svc.Targets(e.ctx())
	noSecret(t, "the list of targets", mustString(l), testKeyID, testSecret)
	if !l.Items[0].CredentialsSet {
		t.Error("credentialsSet is false")
	}
	// the members: a kind that takes none, an unknown member, a value with a space, a missing secret
	for name, c := range map[string]string{
		"an unknown member":   `{"accessKeyId":"AKIAAAAA","secretAccessKey":"abcdefgh","extra":"x"}`,
		"a space":             `{"accessKeyId":"AKIA AAA","secretAccessKey":"abcdefgh"}`,
		"a missing secret":    `{"accessKeyId":"AKIAAAAA"}`,
		"not a string":        `{"accessKeyId":5,"secretAccessKey":"abcdefgh"}`,
		"not an object":       `"secretsecret"`,
		"a short value":       `{"accessKeyId":"AK","secretAccessKey":"abcdefgh"}`,
		"a control character": "{\"accessKeyId\":\"AKIAAAAA\",\"secretAccessKey\":\"abc\\u0001defgh\"}",
	} {
		_, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "n" + name[:3], Config: e.wormConfig(nil), Credentials: []byte(c)})
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			noSecret(t, name, err.Error(), "abcdefgh", "secretsecret")
		}
	}
}
