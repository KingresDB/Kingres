// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package main

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgeranchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
)

// P7R-08: only the answer "I have no file for the target" of a holder says that it has none. Every other error says nothing about the
// file, so that the records of the target wait for a holder that is down and do not fail for good.
func TestAHolderThatCannotBeAskedIsNotAHolderThatHasNoFile(t *testing.T) {
	none := &peerclient.APIError{Status: http.StatusNotFound, Code: "not_found", Message: ledgeranchor.FetchNoneMessage}
	for name, c := range map[string]struct {
		err  error
		want ledgeranchor.HolderAnswer
	}{
		"the holder says it has none":           {none, ledgeranchor.HolderHasNone},
		"the answer is wrapped":                 {fmt.Errorf("fetch: %w", none), ledgeranchor.HolderHasNone},
		"the holder did not answer":             {fmt.Errorf("%w: h2", peerclient.ErrUnreachable), ledgeranchor.HolderUnreached},
		"not ready (503)":                       {&peerclient.APIError{Status: 503, Code: "service_unavailable", Message: "waiting"}, ledgeranchor.HolderUnreached},
		"the file cannot be read (422)":         {&peerclient.APIError{Status: 422, Code: "credentials_invalid", Message: "x"}, ledgeranchor.HolderUnreached},
		"a 404 of a server that lacks the call": {&peerclient.APIError{Status: 404, Code: "not_found", Message: "No such call."}, ledgeranchor.HolderUnreached},
		"another error":                         {errors.New("boom"), ledgeranchor.HolderUnreached},
	} {
		if got := fetchAnswerOf(c.err); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
