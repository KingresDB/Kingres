// SPDX-License-Identifier: PostgreSQL

package problem

import "testing"

// The codes of the instance start and stop operations are in the contract's
// ProblemCode enum with the contract's statuses.
func TestInstanceLifecycleCodes(t *testing.T) {
	for _, tc := range []struct {
		c      Code
		want   string
		status int
	}{
		{InstanceAlreadyRunning, "instance_already_running", 409},
		{InstanceAlreadyStopped, "instance_already_stopped", 409},
		{SystemDatabaseStopped, "system_database_stopped", 503},
	} {
		p := New(tc.c, "d")
		if string(p.Code) != tc.want || p.Status != tc.status || p.Title == "" || p.Type == "" {
			t.Errorf("%s: %+v", tc.want, p)
		}
	}
}
