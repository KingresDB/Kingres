// SPDX-License-Identifier: AGPL-3.0-only

package failover

import "testing"

func TestStateRunningUnknown(t *testing.T) {
	s := State{Group: 1, Instances: []StateInstance{{Group: 1, Running: true}}}
	if s.RunningUnknown() {
		t.Error("an instance that runs is known")
	}
	s.Instances = append(s.Instances, StateInstance{Group: 2, RunningUnknown: true})
	if s.RunningUnknown() {
		t.Error("an instance of another group does not count")
	}
	s.Instances = append(s.Instances, StateInstance{Group: 1, RunningUnknown: true})
	if !s.RunningUnknown() {
		t.Error("an instance of the group that cannot be looked at is not known to be stopped")
	}
}
