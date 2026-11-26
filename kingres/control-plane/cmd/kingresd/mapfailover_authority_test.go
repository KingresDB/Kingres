// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// B8 asks whether this server holds a usable CA key. With the escrow wired in, the answer of the adoption is the answer: a
// ca.key that exists but cannot be used (the adoption put it aside) is not "key held", whatever the file system says.
func TestCheckAuthorityIsTheAnswerOfTheAdoption(t *testing.T) {
	for _, c := range []struct {
		name string
		ok   bool
		err  error
	}{
		{"adopted or held", true, nil},
		{"no usable key and no copy", false, nil},
		{"the copy was refused", false, errors.New("refused")},
	} {
		called := 0
		p := &mapPorts{m: &mapFailover{log: slog.New(slog.DiscardHandler), adoptCA: func(context.Context) (bool, error) {
			called++
			return c.ok, c.err
		}}}
		got, err := p.CheckAuthority(context.Background())
		if err != nil || got != c.ok || called != 1 {
			t.Errorf("%s: got %v %v after %d calls", c.name, got, err, called)
		}
	}
}
