// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// transferConfig is the configuration of the data transfer (contract 0.19.0): the staging directory, the master key, the limits of a task and
// the check of the role between two steps. The staging directory is opened at the first use, so a directory that is not acceptable does not
// keep kingresd from starting; it refuses the transfers, with a message in the log.
func transferConfig(cfg config.Config, authn auth.Authenticator, write func(context.Context, audit.Event)) *ops.TransferConfig {
	bin, _, _ := cluster.ResolveBinDir(cfg.PGBinDir)
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
		MaxDuration: cfg.TransferMaxDuration, MaxAtomicBytes: cfg.MaxAtomicBytes, PGBinDir: bin, KingresVersion: version,
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

// transferMetrics is the Extra of the monitor: the staging use and the counts of the data transfer of this server.
func transferMetrics(o *ops.Ops) func(context.Context) map[string]float64 {
	return func(ctx context.Context) map[string]float64 {
		if o == nil {
			return nil
		}
		used, free, _, running, staged, ok := o.TransferStats(ctx)
		if !ok {
			return nil
		}
		return map[string]float64{"transferStagingUsedBytes": float64(used), "transferStagingFreeBytes": float64(free), "transfersRunning": float64(running), "transfersStaged": float64(staged)}
	}
}

// transferProbe raises the alert transferStagingLow when the reservations hold 90 percent of the quota (critical at 98), or when the file system of the staging
// directory has less free than a tenth of the quota and the reservations still need room.
func transferProbe(o *ops.Ops, host string) func(context.Context, time.Time) ([]monitor.Condition, map[string]bool) {
	return func(ctx context.Context, _ time.Time) ([]monitor.Condition, map[string]bool) {
		if o == nil {
			return nil, nil
		}
		used, _, quota, _, _, ok := o.TransferStats(ctx)
		if !ok || quota <= 0 {
			return nil, nil
		}
		kinds := map[string]bool{"transferStagingLow": true}
		pct := float64(used) * 100 / float64(quota)
		switch {
		case pct >= 98:
			return []monitor.Condition{{Kind: "transferStagingLow", Severity: "critical", Subject: "server:" + host, Message: fmt.Sprintf("The staging area of the data transfer on server %s is %.0f%% of its quota.", host, pct)}}, kinds
		case pct >= 90:
			return []monitor.Condition{{Kind: "transferStagingLow", Severity: "warning", Subject: "server:" + host, Message: fmt.Sprintf("The staging area of the data transfer on server %s is %.0f%% of its quota.", host, pct)}}, kinds
		}
		return nil, kinds
	}
}
