// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// hbaBackupName is the file that holds the access rules of an instance while its writes are stopped for a planned switchover (design
// ha-phase5-part2.md, 5.2 S2): pg_hba.conf then holds only the hold rules, and the backup is what Unquiesce puts back.
const hbaBackupName = "pg_hba.conf.kingres-quiesced"

// HoldRules is the text of pg_hba.conf while an instance is quiesced: only the connections that a switchover needs are kept, the
// replication connections of the standbys and the connections of kingresd itself (its private socket and its loopback address). No client
// and no other node of the extension can open a new session.
func HoldRules(hba string) string {
	var sb strings.Builder
	sb.WriteString("# Held by kingresd for a planned switchover: only replication and kingresd itself are let in.\n")
	for _, line := range strings.Split(hba, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		f := strings.Fields(t)
		if len(f) < 3 {
			continue
		}
		keep := false
		switch {
		case f[0] == "local":
			keep = true
		case strings.Contains(f[1], "replication"):
			keep = true
		case f[2] == ServiceRole && len(f) > 3 && (strings.HasPrefix(f[3], "127.0.0.1/") || strings.HasPrefix(f[3], "::1/")):
			keep = true
		}
		if keep {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// Quiesce replaces the access rules of the instance by the hold rules and reloads it; the rules in force are kept for Unquiesce. It is
// idempotent: a second call leaves the first backup alone.
func (m *Manager) Quiesce(ctx context.Context, id int) error {
	data, err := m.DataDir(id)
	if err != nil {
		return err
	}
	hba := filepath.Join(data, "pg_hba.conf")
	backup := filepath.Join(data, hbaBackupName)
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		cur, rerr := os.ReadFile(hba)
		if rerr != nil {
			return rerr
		}
		if err := writeAtomic(backup, cur); err != nil {
			return err
		}
		if err := writeAtomic(hba, []byte(HoldRules(string(cur)))); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if up, _ := m.Running(ctx, id); up {
		return m.Reload(ctx, id)
	}
	return nil
}

// Unquiesce puts the access rules back and reloads the instance when it runs. Nothing to put back is not an error.
func (m *Manager) Unquiesce(ctx context.Context, id int) error {
	data, err := m.DataDir(id)
	if err != nil {
		return err
	}
	backup := filepath.Join(data, hbaBackupName)
	cur, err := os.ReadFile(backup)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(data, "pg_hba.conf"), cur); err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if up, _ := m.Running(ctx, id); up {
		return m.Reload(ctx, id)
	}
	return nil
}

// Quiesced says that the instance holds the hold rules.
func (m *Manager) Quiesced(id int) bool {
	data, err := m.DataDir(id)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(data, hbaBackupName))
	return err == nil
}
