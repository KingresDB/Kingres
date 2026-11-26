// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package worm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

var secretVerbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"}

func renderAll(vals ...any) []string {
	var out []string
	for _, v := range vals {
		for _, f := range secretVerbs {
			out = append(out, fmt.Sprintf(f, v))
		}
		if j, err := json.Marshal(v); err == nil {
			out = append(out, string(j))
		}
		if e, ok := v.(error); ok {
			for ; e != nil; e = errors.Unwrap(e) {
				out = append(out, e.Error())
			}
		}
	}
	return out
}

// TestNoSecretAnywhere has a store that echoes the credentials in its error
// texts (message, a header-like element) and formats every type and every
// error that the package produces, with every verb and as JSON.
func TestNoSecretAnywhere(t *testing.T) {
	secrets := []string{fakeKeyID, fakeSecret, fakeToken}
	check := func(label string, outs []string) {
		for _, o := range outs {
			for _, s := range secrets {
				if strings.Contains(o, s) {
					t.Fatalf("%s: secret %q found in %q", label, s, o)
				}
				// also a trailing part, in case of truncation
				if strings.Contains(o, s[4:len(s)-3]) {
					t.Fatalf("%s: part of a secret found in %q", label, o)
				}
			}
		}
	}
	var produced []any
	statuses := []struct {
		status int
		code   string
	}{{403, "AccessDenied"}, {400, "InvalidRequest"}, {500, "InternalError"}, {404, "NoSuchBucket"}, {412, "PreconditionFailed"}, {400, "Other"}}
	for _, st := range statuses {
		f := newFakeS3()
		f.echoSecrets = true
		f.putStatus, f.putCode, f.putMsg = st.status, st.code, "problem"
		tg := newTarget(t, f, func(c *Config, o *Options) { o.SessionToken = fakeToken })
		_, err := tg.Submit(context.Background(), testPayload(t, "4", 1))
		if err == nil {
			t.Fatal("expected an error")
		}
		produced = append(produced, err)
		r := tg.Test(context.Background())
		produced = append(produced, r)
		for _, c := range r.Checks {
			produced = append(produced, c)
		}
		f.close()
	}
	// failures on the read side, with echoes
	f := newFakeS3()
	f.echoSecrets = true
	tg := newTarget(t, f, func(c *Config, o *Options) { o.SessionToken = fakeToken })
	p := testPayload(t, "4", 1)
	if _, err := tg.Submit(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	f.getStatus = 500
	_, err := tg.Submit(context.Background(), p)
	produced = append(produced, err)
	f.failSig = true
	_, err = tg.Submit(context.Background(), p)
	produced = append(produced, err)
	// a transport error and an unreachable store
	f.close()
	_, err = tg.Submit(context.Background(), p)
	produced = append(produced, err)
	// a store that echoes the credentials but is otherwise fine: evidence
	g := newFakeS3()
	g.echoSecrets = true
	tg2 := newTarget(t, g, func(c *Config, o *Options) { o.SessionToken = fakeToken })
	rc, _ := mustSubmit(t, tg2, testPayload(t, "5", 1))
	produced = append(produced, rc, tg2, *tg2, tg2.Config(), tg2.s, tg2.s.signer, tg2.s.red)
	g.close()
	// the options with the credentials
	o := Options{Config: tg2.Config(), AccessKeyID: fakeKeyID, SecretAccessKey: fakeSecret, SessionToken: fakeToken}
	produced = append(produced, o, &o)
	for i, v := range produced {
		check(fmt.Sprintf("value %d (%T)", i, v), renderAll(v))
	}
	// the redactor removes what a store echoed
	red := anchor.NewRedactor(anchor.Secret(fakeSecret))
	if got := red.Clean("a "+fakeSecret+" b", 0); strings.Contains(got, fakeSecret) {
		t.Fatal(got)
	}
	// the evidence file carries no credential either
	if strings.Contains(string(rc.Evidence), fakeKeyID) {
		t.Fatal("key id in evidence")
	}
}

func TestStoreMessageIsSanitised(t *testing.T) {
	f := newFakeS3()
	f.putStatus, f.putCode = 500, "InternalError"
	f.putMsg = "line1\nline2 " + strings.Repeat("m", 1000)
	defer f.close()
	_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
	if err == nil || len(err.Error()) > 400 || strings.ContainsAny(err.Error(), "\n\r\t") {
		t.Fatalf("%q", err)
	}
}
