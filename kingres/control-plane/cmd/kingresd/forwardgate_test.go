// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The streaming relay has the same gate: a member, an unreadable catalog, a stopped instance; and its proof is skipped while the instance is
// down only for the two kinds of the forward calls, never for the proof that the leader signs gather assertions with.
func TestForwardStreamGate(t *testing.T) {
	ran := 0
	next := func(context.Context, string, internalapi.ForwardRequest, io.Reader, http.ResponseWriter) error {
		ran++
		return nil
	}
	member := func(_ context.Context, id string) (bool, error) { return id == "db2", nil }
	down := false
	gate := forwardStreamGate(func() bool { return down }, member, next)
	ctx := context.Background()
	call := func(peer string) error {
		return gate(ctx, peer, internalapi.ForwardRequest{}, strings.NewReader(""), httptest.NewRecorder())
	}
	if err := call("db2"); err != nil || ran != 1 {
		t.Fatalf("a member: %v, ran %d", err, ran)
	}
	if err := call("pending"); err == nil || ran != 1 {
		t.Errorf("a server that is not a member: %v, ran %d", err, ran)
	}
	broken := forwardStreamGate(func() bool { return false }, func(context.Context, string) (bool, error) { return false, errors.New("catalog unreadable") }, next)
	if err := broken(ctx, "db2", internalapi.ForwardRequest{}, strings.NewReader(""), httptest.NewRecorder()); err == nil || ran != 1 {
		t.Errorf("an unreadable catalog must refuse: %v, ran %d", err, ran)
	}
	down = true
	if err := call("pending"); err != nil || ran != 2 {
		t.Errorf("while the instance is stopped the pipeline answers: %v, ran %d", err, ran)
	}
	if forwardStreamGate(func() bool { return false }, member, nil) != nil {
		t.Error("a missing backend must stay missing, so that the call answers not_available")
	}
	checked := 0
	check := func(context.Context, fwdproof.Kind, string, internalapi.ForwardRequest) error {
		checked++
		return fwdproof.ErrInvalid
	}
	f := skipWhile(func() bool { return true }, check)
	if err := f(ctx, fwdproof.KindForwardStream, "p", internalapi.ForwardRequest{}); err != nil || checked != 0 {
		t.Errorf("a stream proof while the instance is down: %v %d", err, checked)
	}
	if err := f(ctx, fwdproof.KindGatherAuthorize, "p", internalapi.ForwardRequest{}); !errors.Is(err, fwdproof.ErrInvalid) || checked != 1 {
		t.Errorf("the gather authorization is never skipped: %v %d", err, checked)
	}
}
