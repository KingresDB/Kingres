// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package instance

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MemorySettings returns the PostgreSQL settings derived from the memory quota
// of a server in MiB: a quarter of it for shared_buffers, half for the planner's
// cache estimate and a sixteenth (at most 2 GiB) for maintenance work.
func MemorySettings(mb int) map[string]string {
	return map[string]string{
		"shared_buffers":       strconv.Itoa(mb/4) + "MB",
		"effective_cache_size": strconv.Itoa(mb/2) + "MB",
		"maintenance_work_mem": strconv.Itoa(min(mb/16, 2048)) + "MB",
	}
}

// PostmasterToken identifies the running postmaster of instance id by the
// process id and the start time recorded in postmaster.pid; it is "" when the
// instance is not running. A change of the managed configuration that needs a
// restart is pending for as long as the token is the one it was when the change
// was saved (a restarted instance has another start time even if the process id
// is reused).
func (m *Manager) PostmasterToken(id int) string {
	data, err := m.DataDir(id)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(data, "postmaster.pid")) //nolint:gosec // path below the manager root
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 3 {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid < 1 {
		return ""
	}
	return strconv.Itoa(pid) + ":" + strings.TrimSpace(lines[2])
}
