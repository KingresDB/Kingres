// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The acknowledgement of a diverged chain (phase 7, step 20; design 9.4): the task ledger.epoch.start closes the old epoch and opens a new log.

// KindLedgerEpoch is the task that starts a new epoch of a diverged chain.
const KindLedgerEpoch = "ledger.epoch.start"

type epochParams struct {
	Database string `json:"database"`
	Group    int32  `json:"group"`
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
	Actor    string `json:"actor"`
}

// epochKindOf maps the kind of the contract to the one the event carries.
func epochKindOf(k string) string {
	if k == "failoverLoss" {
		return "failover-loss"
	}
	return k
}

// PrepareEpoch validates the acknowledgement and checks that the chain is diverged (409 chain_not_diverged otherwise).
func (o *Ops) PrepareEpoch(ctx context.Context, db string, group int32, actor string, req api.AcknowledgeLedgerDivergenceRequest) (any, error) {
	if _, err := ledgerDB(db); err != nil {
		return nil, err
	}
	var errs []problem.FieldError
	if req.Confirm != db {
		errs = append(errs, field("/confirm", "This is not the name of the database.", "mismatch"))
	}
	switch req.Kind {
	case "restore", "failoverLoss", "other":
	default:
		errs = append(errs, field("/kind", "must be restore, failoverLoss or other", "enum"))
	}
	if n := len([]rune(req.Reason)); n < 1 || n > 200 {
		errs = append(errs, field("/reason", "Give a reason of 1 to 200 characters.", "length"))
	} else if strings.IndexFunc(req.Reason, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
		errs = append(errs, field("/reason", "The reason is published in a signed event: printable ASCII only.", "ascii"))
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The request is invalid.", errs...)
	}
	if o.LedgerChainDiverged == nil || o.LedgerEpochStart == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server does not publish the ledger.")
	}
	ok, err := o.LedgerChainDiverged(ctx, db, group)
	if err != nil {
		return nil, wrap(err)
	}
	if !ok {
		return nil, problem.New(problem.ChainNotDiverged, "The chain of this shard group has not diverged.")
	}
	return epochParams{Database: db, Group: group, Kind: epochKindOf(req.Kind), Reason: req.Reason, Actor: actor}, nil
}

func (o *Ops) ledgerEpochDefinition() task.Definition {
	return task.Definition{Kind: KindLedgerEpoch, Steps: []task.Step{{Name: "Close the old epoch and open the new one", Run: func(ctx context.Context, env *task.Env) error {
		p, err := inputOf[epochParams](env.Task)
		if err != nil {
			return err
		}
		if o.LedgerEpochStart == nil {
			return errors.New("this server does not publish the ledger")
		}
		if err := o.LedgerEpochStart(ctx, p.Database, p.Group, p.Kind, p.Reason, p.Actor); err != nil {
			return fmt.Errorf("starting the new epoch of shard group %d: %w", p.Group, err)
		}
		return nil
	}}}}
}
