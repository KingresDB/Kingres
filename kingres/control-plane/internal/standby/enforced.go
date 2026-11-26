// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// hasStandby tells whether a standby exists that a failover may use: an entry of the group that is Ready, or one that was ready once and is being rebuilt
// (generation above 0). It is the one predicate of the lease fence: Agent.HasStandby (the run time decision) and EnforcedOnDisk (the decision of a cold
// start) both use it, so that they cannot disagree.
func (s state) hasStandby(g int32) bool {
	for _, e := range s.entriesOfGroup(g) {
		if e.Ready || e.Generation > 0 {
			return true
		}
	}
	return false
}

// EnforcedOnDisk reads the standby list that this server keeps below dataDir (the file the agent loads at its start) and returns the predicate "the lease
// of this group is enforced", the one Agent.HasStandby answers at run time. It is meant for the start of kingresd, before the agent exists (lease.PrepareStart).
// A list that was never written is no standby: nothing can be promoted, so nothing is protected. A list that exists but cannot be read or parsed is
// reported with an error and the predicate then says true for every group: not knowing is judged as enforced (a primary then waits for its lease before it
// starts). That never blocks a start that would work, because kingresd does not start with such a list either (NewAgent refuses it).
func EnforcedOnDisk(dataDir string) (func(group int32) bool, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "standby", stateFile)) //nolint:gosec // fixed name below the data directory
	if errors.Is(err, os.ErrNotExist) {
		return func(int32) bool { return false }, nil
	}
	if err != nil {
		return func(int32) bool { return true }, fmt.Errorf("standby: reading %s: %w", stateFile, err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return func(int32) bool { return true }, fmt.Errorf("standby: %s is not valid: %w", stateFile, err)
	}
	return st.hasStandby, nil
}
