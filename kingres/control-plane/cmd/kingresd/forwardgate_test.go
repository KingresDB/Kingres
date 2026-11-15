// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// CP2-01: a server that holds a certificate but is not a member may not forward;
// while the local instance is stopped the pipeline answers (nothing is read).
func TestForwardGateRefusesAServerThatIsNotAMember(t *testing.T) {
	ran := 0
	next := func(context.Context, string, internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
		ran++
		return internalapi.ForwardResponse{Status: 200}, nil
	}
	members := map[string]bool{"db2": true}
	member := func(_ context.Context, id string) (bool, error) { return members[id], nil }
	down := false
	gate := forwardGate(func() bool { return down }, member, next)
	ctx := context.Background()
	if _, err := gate(ctx, "db2", internalapi.ForwardRequest{}); err != nil || ran != 1 {
		t.Fatalf("a member: %v, ran %d", err, ran)
	}
	if _, err := gate(ctx, "pending", internalapi.ForwardRequest{}); err == nil || ran != 1 {
		t.Errorf("a server that is not a member: %v, ran %d", err, ran)
	}
	broken := forwardGate(func() bool { return false }, func(context.Context, string) (bool, error) { return false, errors.New("catalog unreadable") }, next)
	if _, err := broken(ctx, "db2", internalapi.ForwardRequest{}); err == nil || ran != 1 {
		t.Errorf("an unreadable catalog must refuse: %v, ran %d", err, ran)
	}
	down = true
	if _, err := gate(ctx, "pending", internalapi.ForwardRequest{}); err != nil || ran != 2 {
		t.Errorf("while the instance is stopped the pipeline answers: %v, ran %d", err, ran)
	}
}

func TestSkipWhileOnlySkipsWhileTheInstanceIsDown(t *testing.T) {
	called := 0
	check := func(context.Context, fwdproof.Kind, string, internalapi.ForwardRequest) error {
		called++
		return fwdproof.ErrInvalid
	}
	down := true
	f := skipWhile(func() bool { return down }, check)
	if err := f(context.Background(), fwdproof.KindForward, "p", internalapi.ForwardRequest{}); err != nil || called != 0 {
		t.Errorf("skipped: %v %d", err, called)
	}
	down = false
	if err := f(context.Background(), fwdproof.KindForward, "p", internalapi.ForwardRequest{}); !errors.Is(err, fwdproof.ErrInvalid) || called != 1 {
		t.Errorf("checked: %v %d", err, called)
	}
	// Without a key source nothing can be proven.
	if err := newIdentityChecker(nil, "me")(context.Background(), fwdproof.KindForward, "p", internalapi.ForwardRequest{}); !errors.Is(err, fwdproof.ErrInvalid) {
		t.Errorf("no key source: %v", err)
	}
}
