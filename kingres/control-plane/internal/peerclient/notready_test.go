// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package peerclient

import (
	"errors"
	"testing"
)

func TestServiceUnavailableMatchesNotReady(t *testing.T) {
	if !errors.Is(&APIError{Status: 503, Code: "service_unavailable"}, ErrNotReady) {
		t.Error("503 service_unavailable is transient")
	}
	for _, e := range []*APIError{{Status: 403, Code: "not_leader"}, {Status: 503, Code: "unavailable"}, {Status: 409, Code: "service_unavailable"}} {
		if errors.Is(e, ErrNotReady) {
			t.Errorf("%v must not be transient", e)
		}
	}
}
