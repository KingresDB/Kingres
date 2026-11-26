// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

const planted = "PLANTEDSECRET-9f8e7d6c"

func formats(v any) string {
	var b strings.Builder
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		fmt.Fprintf(&b, f+"\n", v)
	}
	if j, err := json.Marshal(v); err == nil {
		b.Write(j)
	}
	return b.String()
}

func noSecret(t *testing.T, what string, v any) {
	t.Helper()
	if s := formats(v); strings.Contains(s, planted) {
		t.Errorf("%s leaks the secret: %.300s", what, s)
	}
	if err, ok := v.(error); ok {
		for e := err; e != nil; e = errors.Unwrap(e) {
			if strings.Contains(e.Error(), planted) {
				t.Errorf("%s: wrapped error leaks the secret", what)
			}
		}
	}
}

// A secret in the calendar URL (user information or query) is refused by the
// configuration and never printed; a secret echoed by a calendar in its error
// body is never carried into an error, a check or a receipt.
func TestNoSecretAnywhere(t *testing.T) {
	for _, u := range []string{
		"https://user:" + planted + "@a.example",
		"https://a.example/?token=" + planted,
		"https://a.example/#" + planted,
		"https://a.example/" + planted + "%2f",
	} {
		_, err := ParseConfig(json.RawMessage(`{"calendars":["` + u + `"]}`))
		if err == nil {
			t.Errorf("accepted %q", u)
			continue
		}
		noSecret(t, "config error", err)
		_, err = NormalizeCalendarURL(u, true)
		noSecret(t, "url error", err)
		_, err = New(Config{Calendars: []string{u}}, Options{})
		noSecret(t, "New error", err)
	}

	p := testPayload(t)
	for _, mode := range []string{"500", "403", "429", "huge", "cut", "garbage", "noattest"} {
		a, b := newFakeCal(t, false), newFakeCal(t, false)
		a.echo, b.echo = planted, planted
		a.set(mode)
		b.set("ok")
		tg := newTarget(t, nil, 2, a.url(), b.url())
		rc, err := tg.Submit(context.Background(), p)
		noSecret(t, "submit "+mode+" error", err)
		var pe *PartialError
		if errors.As(err, &pe) {
			noSecret(t, "partial error", pe)
			noSecret(t, "partial evidence", pe.Evidence)
		}
		noSecret(t, "receipt", rc)
		noSecret(t, "target", tg)
		noSecret(t, "config", tg.Config())
		res := tg.Test(context.Background())
		noSecret(t, "test result "+mode, res)
		for _, c := range res.Checks {
			noSecret(t, "check", c)
		}
		// upgrade errors
		b.set(mode)
		if rc2, err := newTarget(t, nil, 1, b.url()).Submit(context.Background(), p); err == nil {
			_, err = tg.Upgrade(context.Background(), rc2)
			noSecret(t, "upgrade error", err)
		}
	}
	// the full anchor error types
	e := anchor.Errorf(anchor.ClassPermanent, "ots", "x", "y", "text")
	noSecret(t, "anchor error", e)
	noSecret(t, "options", Options{UserAgent: "kingresd"})
}
