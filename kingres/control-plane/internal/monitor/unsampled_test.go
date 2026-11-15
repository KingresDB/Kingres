// SPDX-License-Identifier: AGPL-3.0-only

package monitor

import (
	"testing"
	"time"
)

// A metric nobody samples (the client entry point's while it is off) is the cluster
// series with null points, never zeros; grouped, it is no series (contract 0.9.0).
func TestAssembleOfAnUnsampledMetric(t *testing.T) {
	from := time.Unix(1000, 0)
	res := Assemble(from, 10, 3, nil, "", "", "", true)
	if len(res) != 1 || len(res[0].Points) != 3 {
		t.Fatalf("%+v", res)
	}
	for _, p := range res[0].Points {
		if p.V != nil {
			t.Errorf("a point has a value: %v", *p.V)
		}
	}
	if res := Assemble(from, 10, 3, nil, "", "", "server", true); len(res) != 0 {
		t.Errorf("grouped: %+v", res)
	}
}

func TestParseSubjectTask(t *testing.T) {
	s := parseSubject("task:118")
	if s.TaskID == nil || *s.TaskID != 118 || s.Aspect != nil {
		t.Errorf("%+v", s)
	}
}
