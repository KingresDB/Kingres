// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

func (w *world) followArgs(out string, more ...string) []string {
	return append([]string{"trust", "follow", "--trust", w.trust, "--source", w.dir, "--out", out, "--now", "2026-11-19T12:00:00Z"}, more...)
}

func newOut(t *testing.T) string { return filepath.Join(testdir.New(t), "new-trust.json") }

func loadTrust(t *testing.T, p string) *trust.File {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := trust.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTrustFollowRotationRetirementAndRevocation(t *testing.T) {
	w := newWorld(t)
	s3 := newSigner(3)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	// K2 is added: signed by K1 (authorises) and K2 (proves possession)
	w.event(1, "key-added", []signer{w.s1, w.s2}, keyAddedLines(w.s2)...)
	out := newOut(t)
	wantRun(t, 0, w.followArgs(out), "key-added", "2 event(s) read and verified")
	f := loadTrust(t, out)
	if len(f.Keys) != 2 || f.Keys[1].Fingerprint != w.s2.fp() || f.Keys[1].NotBefore == nil {
		t.Fatalf("%+v", f.Keys)
	}
	// the file that was read is not changed, and the new file is never overwritten
	if len(loadTrust(t, w.trust).Keys) != 1 {
		t.Fatal("the input trust file changed")
	}
	wantRun(t, 2, w.followArgs(out), "exists")
	wantRun(t, 2, w.followArgs(w.trust), "never over it")
	// K1 retired by K2 with a bound
	w.event(2, "key-retired", []signer{w.s2}, "key: "+base64.StdEncoding.EncodeToString(w.s1.pub), "max-tree-size: 77")
	out2 := newOut(t)
	wantRun(t, 0, w.followArgs(out2), "key-retired", "tree size 77 or less")
	f = loadTrust(t, out2)
	if f.Keys[0].MaxTreeSize == nil || *f.Keys[0].MaxTreeSize != 77 {
		t.Fatalf("%+v", f.Keys[0])
	}
	// an unbounded retirement leaves the key usable, and says so
	w.event(2, "key-retired", []signer{w.s2}, "key: "+base64.StdEncoding.EncodeToString(w.s1.pub), "max-tree-size: unbounded")
	wantRun(t, 0, w.followArgs(newOut(t)), "no bound")
	// K3 added by K2 (a key added by an event authorises from then on) and K1 revoked by K3
	w.event(2, "key-retired", []signer{w.s2}, "key: "+base64.StdEncoding.EncodeToString(w.s1.pub), "max-tree-size: 77")
	w.event(3, "key-added", []signer{w.s2, s3}, keyAddedLines(s3)...)
	w.event(4, "key-revoked", []signer{s3}, "key: "+base64.StdEncoding.EncodeToString(w.s1.pub), "compromised-since: 2026-11-18T00:00:00Z", "reason: stolen laptop")
	out3 := newOut(t)
	wantRun(t, 0, w.followArgs(out3), "key-revoked", "stolen laptop")
	f = loadTrust(t, out3)
	if !f.Keys[0].Revoked || f.Keys[0].MaxTreeSize != nil || len(f.Keys) != 3 {
		t.Fatalf("%+v", f.Keys)
	}
	// following the output again is the same (an added key is pinned now), as long as no key it needs is revoked in it
	w2 := *w
	w2.trust = out2
	wantRun(t, 0, w2.followArgs(newOut(t)), "VERIFIED:")
}

func TestTrustFollowRefusesWhatNoAuthorisedKeySigned(t *testing.T) {
	cases := []struct {
		name  string
		build func(w *world)
		want  string
	}{
		{"an unpinned key adds itself", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			w.event(1, "key-added", []signer{w.s2}, keyAddedLines(w.s2)...)
		}, "no key that counts here"},
		{"a key added by an unpinned key", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			s3, s4 := newSigner(3), newSigner(4)
			w.event(1, "key-added", []signer{s3, s4}, keyAddedLines(s4)...)
		}, "no key that counts here"},
		{"the new key does not sign its own event", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			w.event(1, "key-added", []signer{w.s1}, keyAddedLines(w.s2)...)
		}, "must sign its own event"},
		{"a key of small order", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			small := signer{pub: append([]byte{1}, make([]byte, 31)...)}
			w.event(1, "key-added", []signer{w.s1}, "key: "+base64.StdEncoding.EncodeToString(small.pub), "fingerprint: "+small.fp(), "not-before: 2026-11-19T00:00:00Z")
		}, "small order"},
		{"a wrong fingerprint line", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			w.event(1, "key-added", []signer{w.s1, w.s2}, "key: "+base64.StdEncoding.EncodeToString(w.s2.pub), "fingerprint: "+strings.Repeat("ab", 32), "not-before: 2026-11-19T00:00:00Z")
		}, "fingerprint line"},
		{"a key retires itself", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			w.event(1, "key-retired", []signer{w.s1}, "key: "+base64.StdEncoding.EncodeToString(w.s1.pub), "max-tree-size: 5")
		}, "another authorised key"},
		{"a revocation signed only by the revoked key", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			w.event(1, "key-revoked", []signer{w.s1}, "key: "+base64.StdEncoding.EncodeToString(w.s1.pub), "compromised-since: 2026-11-18T00:00:00Z", "reason: x")
		}, "different authorised key"},
		{"a retirement of a key nobody introduced", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			w.event(1, "key-retired", []signer{w.s1}, "key: "+base64.StdEncoding.EncodeToString(w.s2.pub), "max-tree-size: 5")
		}, "no earlier event or the trust file introduced"},
		{"a number that is not the file's", func(w *world) {
			w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
			w.put("events/000001", []byte(signText(eventText(5, "key-added", keyAddedLines(w.s2)...), w.prefix, w.s1, w.s2)))
		}, "number 5"},
		{"a signature that is wrong", func(w *world) {
			n := signText(eventText(0, "key-added", keyAddedLines(w.s1)...), w.prefix, w.s1)
			lines := strings.Split(n, "\n")
			// damage the signature line: change a character inside the base64 payload
			l := lines[len(lines)-2]
			i := len(l) - 5
			c := byte('A')
			if l[i] == 'A' {
				c = 'B'
			}
			lines[len(lines)-2] = l[:i] + string(c) + l[i+1:]
			w.put("events/000000", []byte(strings.Join(lines, "\n")))
		}, "does not verify"},
		{"an event that is not UTF-8 text", func(w *world) {
			w.put("events/000000", []byte("\xff\xfe"))
		}, "not a signed note"},
		{"an unsigned note", func(w *world) {
			w.put("events/000000", []byte(eventText(0, "key-added", keyAddedLines(w.s1)...)+"\n"))
		}, "not a signed note"},
	}
	for _, c := range cases {
		w := newWorld(t)
		c.build(w)
		wantRun(t, 1, w.followArgs(newOut(t)), "FAILED at step events", c.want)
	}
}

func TestTrustFollowWithNothingToFollow(t *testing.T) {
	w := newWorld(t)
	out := newOut(t)
	wantRun(t, 3, w.followArgs(out), "INCOMPLETE", "no event")
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a file was written for an incomplete run")
	}
	wantRun(t, 2, []string{"trust", "follow", "--trust", w.trust, "--source", w.dir}, "missing --out")
}

func TestKeyringOfThePublicationIsNeverASourceOfFirstTrust(t *testing.T) {
	// a publication with a keyring.json and events signed by a key the auditor did not pin: nothing is followed
	w := newWorld(t)
	w.put("keyring.json", []byte(`{"format":1}`))
	w.event(0, "key-added", []signer{w.s2}, keyAddedLines(w.s2)...)
	wantRun(t, 1, w.followArgs(newOut(t)), "FAILED at step events")
}

func TestTrustFollowAKeyThatWasRevokedVouchesForNothingAfterIt(t *testing.T) {
	w := newWorld(t)
	s3 := newSigner(3)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "key-added", []signer{w.s1, w.s2}, keyAddedLines(w.s2)...)
	w.event(2, "key-revoked", []signer{w.s2}, "key: "+base64.StdEncoding.EncodeToString(w.s1.pub), "compromised-since: 2026-11-18T00:00:00Z", "reason: stolen")
	// K3 is added with the signature of the revoked K1 and K3 itself: nothing authorised vouches for it
	w.event(3, "key-added", []signer{w.s1, s3}, keyAddedLines(s3)...)
	wantRun(t, 1, w.followArgs(newOut(t)), "FAILED at step events", "event 000003", "no key that counts here")
	// the same event signed by K2 is fine
	w.event(3, "key-added", []signer{w.s2, s3}, keyAddedLines(s3)...)
	wantRun(t, 0, w.followArgs(newOut(t)), "VERIFIED:")
}
