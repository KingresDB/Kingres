// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// transferConfig is the configuration of the data transfer (contract 0.19.0): the staging directory, the master key, the limits of a task and
// the check of the role between two steps. The staging directory is opened at the first use, so a directory that is not acceptable does not
// keep kingresd from starting; it refuses the transfers, with a message in the log.
func transferConfig(cfg config.Config, authn auth.Authenticator, write func(context.Context, audit.Event)) *ops.TransferConfig {
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	return &ops.TransferConfig{
		Dir: abs(cfg.TransferDir), KeyFile: abs(cfg.TransferKeyFile),
		// The staging directory may not be or hold the data directory of kingresd (the instances, the keys, the audit log live there).
		Forbidden:   []string{abs(cfg.DataDir), abs(filepath.Join(cfg.DataDir, "instances"))},
		MaxDuration: cfg.TransferMaxDuration, MaxAtomicBytes: cfg.MaxAtomicBytes,
		CurrentRoles: func(ctx context.Context, role string) ([]string, error) { return authn.CurrentRoles(ctx, role) },
		Audit: func(ctx context.Context, action string, details map[string]any) {
			write(ctx, audit.Event{Actor: "system", Source: "system", Action: action, ResourceType: "transfer", Outcome: audit.OutcomeSuccess, Details: details})
		},
	}
}

// transferJanitorJob is the janitor of the staged files: every 5 minutes on the manager leader (design "Where files live").
func transferJanitorJob(o *ops.Ops) scheduler.Job {
	return scheduler.Job{Name: "transfer.janitor", Every: 5 * time.Minute, Jitter: 0.1, Run: func(ctx context.Context, _ scheduler.Term) error {
		if o == nil || o.Transfer == nil {
			return nil
		}
		_, err := o.TransferSweep(ctx)
		return err
	}}
}
