// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/standby"
)

// The local recovery of a map group that is held back for good (phase 5, the case that no server can end): a planned switchover or a hard failover of the map group stopped
// the primary of the map group (the system database) on this server and held it back for the server that the group was handed to; that server died before it
// promoted, and the cluster has no second standby of the map group that could show that nothing was promoted. The REST API answers 503 on every server (it needs the
// system database), the abandon call needs the system database, and the internal calls that end a hold are calls between servers that refuse to end it without that
// proof. The operator then runs on the server of the old leader, with kingresd stopped:
//
//	kingresd -release-map-hold -lost <serverId> -accept-data-loss <the settings of the server>
//
// which removes the hand-over markers of the map group of this server, so that the lease rounds start the old primary when this server holds the lease. It is the
// operator's word that the named server is lost and will not be started again: whatever it may have promoted and written is given up (the server must be removed from
// the cluster, or rebuilt, and never started with its old data directory). Nothing else is changed.

// releaseHoldArgs are the three words of the command; they are no settings of the server.
type releaseHoldArgs struct {
	requested bool
	lost      string
	accept    bool
}

// splitReleaseHold takes the words of the command out of the arguments of kingresd: -release-map-hold, -lost <serverId> (or -lost=<serverId>) and -accept-data-loss.
func splitReleaseHold(args []string) (rest []string, r releaseHoldArgs, err error) {
	flagName := func(a string) string { return strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-") }
	isFlag := func(a string) bool { return strings.HasPrefix(a, "-") && len(a) > 1 }
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !isFlag(a) {
			rest = append(rest, a)
			continue
		}
		switch flagName(a) {
		case "release-map-hold":
			r.requested = true
		case "accept-data-loss":
			r.accept = true
		case "lost":
			if v, ok := strings.CutPrefix(strings.TrimLeft(a, "-"), "lost="); ok {
				r.lost = v
			} else if i+1 < len(args) {
				i++
				r.lost = args[i]
			} else {
				return nil, r, errors.New("-lost needs the id of the server")
			}
		default:
			rest = append(rest, a)
		}
	}
	if !r.requested && (r.lost != "" || r.accept) {
		return nil, r, errors.New("-lost and -accept-data-loss belong to -release-map-hold")
	}
	return rest, r, nil
}

// auditActionReleaseMapGroupHold is the action of the audit event that the command writes (contract 0.34.0).
const auditActionReleaseMapGroupHold = "releaseMapGroupHold"

// releaseMapHold is the command. It refuses unless the server is declared lost and the loss of data accepted, unless the server is not this one, unless the data
// directory is the one of a KingresDB server, and unless nothing else is using it (kingresd is stopped).
//
// The command is repeatable: the markers go first, then the freeze of the synchronous list, and a run that finds the hold gone but the wait marker still there (an
// earlier run that ended between the two) finishes the freeze and says so. Its audit record is written after the change, with its outcome: a crash in between leaves a
// released hold without a record, which the log line and the wait marker still show; the other order would record a release that did not happen.
func releaseMapHold(cfg config.Config, r releaseHoldArgs, log *slog.Logger, out io.Writer) error {
	if r.lost == "" || !r.accept {
		return errors.New("-release-map-hold needs both -lost <serverId>, the server that the map group was handed to and that is lost, and -accept-data-loss, your word that what that server may have promoted and written is given up")
	}
	self := cfg.ServerHost
	if self == "" {
		host, err := os.Hostname()
		if err != nil {
			host = "localhost"
		}
		self = host
	}
	if r.lost == self {
		return fmt.Errorf("-lost names this server (%s): the server that is lost cannot be the one whose hold is released; run the command on the server of the old leader", self)
	}
	leaseDir := filepath.Join(cfg.DataDir, leaseDirName)
	if st, err := os.Stat(leaseDir); err != nil || !st.IsDir() {
		// Nothing is created for a mistyped -data-dir: no lock file, no audit log.
		return fmt.Errorf("%s is not the data directory of a KingresDB server (it has no %s directory)", cfg.DataDir, leaseDirName)
	}
	unlock, err := lockDataDirOpts(cfg.DataDir, 0, false)
	if err != nil {
		return fmt.Errorf("%w: stop kingresd on this server first (data directory %s)", err, cfg.DataDir)
	}
	defer unlock()
	sink, err := audit.OpenFile(cfg.AuditFile, time.Now)
	if err != nil {
		return fmt.Errorf("opening the audit log: %w", err)
	}
	defer sink.Close()
	ctx := context.Background()
	if err := sink.Ready(ctx); err != nil {
		return fmt.Errorf("the audit log cannot take a record, so nothing is changed: %w", err)
	}
	// A bootstrap that this server runs or ran is not ours to end by a marker: it is given up through the call that does it.
	if js, jerr := journal.Open(filepath.Join(cfg.DataDir, "failover")); jerr != nil {
		return fmt.Errorf("reading the journal of the map group failover: %w", jerr)
	} else if js.Active() {
		return errors.New("a bootstrap of the map group is in progress on this server (its journal is active); start kingresd and give it up with abandonMapGroupBootstrap")
	}
	const group = 0
	candidate, rerr := lease.ReleaseHold(leaseDir, group, r.lost)
	completed := false
	if errors.Is(rerr, lease.ErrNotHeldBack) && lease.Waiting(leaseDir, group) {
		// An earlier run released the hold and ended before it lifted the freeze: this run finishes it.
		rerr, completed = nil, true
	}
	freezeCleared := false
	if rerr == nil {
		// the failover that froze the synchronous list is given up with the hold; a frozen list would keep the primary waiting for the lost standby until the freeze lapses
		cleared, ferr := standby.ClearFreezeOnDisk(cfg.DataDir, group)
		freezeCleared = cleared
		if ferr != nil {
			rerr = fmt.Errorf("the hold is released but the freeze of the synchronous standby list could not be lifted (run the command again): %w", ferr)
		}
	}
	if completed && !freezeCleared && rerr == nil {
		rerr = lease.ErrNotHeldBack // the hold is gone and there was no freeze left to lift: nothing to do
	}
	details := map[string]any{"lostServerId": r.lost, "serverId": self, "acceptDataLoss": true, "recordedCandidate": candidate, "freezeCleared": freezeCleared, "completedEarlierRun": completed}
	if u, uerr := user.Current(); uerr == nil {
		details["osUser"] = u.Username
	}
	outcome := audit.OutcomeSuccess
	if rerr != nil {
		outcome = audit.OutcomeFailure
		details["error"] = rerr.Error()
	}
	_, aerr := sink.Append(ctx, audit.Event{Actor: "system", Source: "cli", Action: auditActionReleaseMapGroupHold, ResourceType: "shardGroup", ResourceID: "0", Outcome: outcome, Details: details})
	if rerr != nil {
		if aerr != nil {
			log.Error("the audit record of the refused release could not be written", "error", aerr)
		}
		return rerr
	}
	log.Warn("the hold of the primary of the map group was released by the operator: the server that the group was handed to is declared lost and the loss of data accepted; start kingresd, the primary starts when this server holds the lease",
		"lostServer", r.lost, "server", self, "completedEarlierRun", completed)
	fmt.Fprintf(out, "The hold of the primary of the map group on %s is released. Server %s is declared lost: never start it again with its old data directory; remove it from the cluster afterwards.\nStart kingresd now; the primary of the map group starts when this server holds its lease.\n", self, r.lost)
	if aerr != nil {
		return fmt.Errorf("the hold is released, but the audit record could not be written: %w", aerr)
	}
	return nil
}
