// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ops

import (
	"encoding/json"
	"testing"
)

func parseUpdate(t *testing.T, body string) (float64, bool, int) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	req, errs := ParseServerUpdate(raw)
	if req.Weight == nil {
		return 0, false, len(errs)
	}
	return *req.Weight, true, len(errs)
}

func TestParseServerUpdateWeight(t *testing.T) {
	if w, ok, n := parseUpdate(t, `{"weight": 2.5}`); !ok || w != 2.5 || n != 0 {
		t.Fatalf("weight 2.5: %v %v %d", w, ok, n)
	}
	for _, body := range []string{`{"weight": 0}`, `{"weight": -1}`, `{"weight": 1000.5}`, `{"weight": "2"}`, `{"weight": null}`} {
		if _, ok, n := parseUpdate(t, body); ok || n != 1 {
			t.Errorf("%s: want one error and no weight, got ok=%v errors=%d", body, ok, n)
		}
	}
	if _, _, n := parseUpdate(t, `{"weight": 1000}`); n != 0 {
		t.Error("1000 is allowed")
	}
	if _, _, n := parseUpdate(t, `{"weight": 2, "shouldHaveShards": true, "serverGroup": null}`); n != 0 {
		t.Error("the three members go together")
	}
}
