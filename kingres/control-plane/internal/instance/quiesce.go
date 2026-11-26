// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// hbaBackupName is the file that holds the access rules of an instance while its writes are stopped for a planned switchover (design
// ha-phase5-part2.md, 5.2 S2): pg_hba.conf then holds only the hold rules, and the backup is what Unquiesce puts back. While it exists
// the backup, not pg_hba.conf, is the truth about the rules: whoever reads the rules to copy them reads the backup (rulesAt), and whoever
// writes new rules writes the backup and puts the hold rules over them (writeRulesAt) (F4-10).
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
		case isReplicationField(f[1]):
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

// isReplicationField says that the database field of a rule is the replication keyword (alone or in a comma list), not a database whose
// name merely contains it.
func isReplicationField(db string) bool {
	for _, d := range strings.Split(db, ",") {
		if strings.TrimSpace(d) == "replication" {
			return true
		}
	}
	return false
}

// rulesAt reads the access rules in force of the data directory: the backup while the directory is held, pg_hba.conf otherwise.
func rulesAt(data string) ([]byte, error) {
	if b, err := os.ReadFile(filepath.Join(data, hbaBackupName)); err == nil { //nolint:gosec // a fixed name below the manager root
		return b, nil
	}
	return os.ReadFile(filepath.Join(data, "pg_hba.conf")) //nolint:gosec // a fixed name below the manager root
}

// writeRulesAt writes the access rules of the data directory: pg_hba.conf, or, while the directory is held, the backup (the rules to put
// back) with the hold rules over them.
func writeRulesAt(data, text string) error {
	hba := filepath.Join(data, "pg_hba.conf")
	backup := filepath.Join(data, hbaBackupName)
	if _, err := os.Stat(backup); err == nil {
		if err := writeAtomic(backup, []byte(text)); err != nil {
			return err
		}
		return writeAtomic(hba, []byte(HoldRules(text)))
	}
	return writeAtomic(hba, []byte(text))
}

// dropHold removes the backup of a directory that is not a primary (any more): the rebuild of an old primary wrote the rules it needs, and
// the rules of the primary that was held must not be put over them.
func dropHold(data string) {
	_ = os.Remove(filepath.Join(data, hbaBackupName))
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
	if !m.IsWritable(id) {
		// The instance is a standby now (the old primary of a switchover, rebuilt as a copy of the new primary): the rules of the
		// primary that was held are not put over the ones the rebuild wrote.
		dropHold(data)
		return nil
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
