// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
)

// The restore of the system database (design 9.4, "Restoring only the system database", P7A-02).
//
// What a restore costs, and what the publisher can and cannot do about it. The aggregate leaf and the events carry the clock time and what the
// chains looked like at that moment, so they cannot be derived again from the chains: a restored database that holds fewer of them cannot be
// brought back to what was published by any computation, and a publisher that signed the next size of such a log would sign another root for a
// size that was published (the fork that the record of what was published, P8-01, exists to prevent). The publisher therefore refuses, with a
// critical alert, for as long as the database is behind or contradicts the record. That refusal is the right default and has an end only
// through a deliberate act of an operator, which is this:
//
//  1. The alert names a confirmation: sixteen hexadecimal digits that depend on the record and on what the restored database holds, so that
//     they fit this one situation. The operator checks that the database really was restored (the alert says what was published and what the
//     database holds), and writes the confirmation, and optionally a reason on a second line, in the file accept-restore in the key
//     directory of the manager leader. Nothing else accepts the loss: no setting, no call, no removal of the record.
//  2. The collector, finding the confirmation in the file, replaces the record by what the restored database holds, one generation higher
//     (ledgerstate.File.Accept; the record that was replaced is kept beside it), removes the file, and goes on. A record of a higher generation
//     replaces the records of the other holders when it reaches them (ledgerstate Merge); a holder that missed it is never able to take the
//     floor back.
//  3. In the first round after the acceptance, and in every round until it succeeds, the signed event restore-acknowledged is published: the
//     generation, the counts and the SHA-256 sums of what the record held and of what the database holds, the reason, and who accepted. It is
//     the public statement that the history of the logs listed in the old record was cut and continues from the restored database. A round
//     that cannot publish it (no key can sign now) signs nothing else.
//
// What the publication then is. The logs continue from what the restored database holds: the checkpoints and the events from there on are new
// content for numbers and sizes that were published before. A mirror or an auditor that holds the old checkpoints sees exactly that, as a fork
// with the event next to it that says why; that is the point of the event and of mirrors, and the operator is told so before the confirmation is
// asked for. Without this act nothing that was published is signed again with other content. The chains that were published and are still in the
// shard groups are caught up by the next rounds as for any log (their leaves are the blocks, which are not lost), so for them the cut is only in
// what the restored database had stored and not in what the chains hold; the aggregate log and the events are where the history is cut.
// The design text of 9.4 that promised that the same leaves give the same bytes held for the chain logs only.

// EventRestoreAcknowledged is the type of the event that announces an accepted restore of the system database.
const EventRestoreAcknowledged = "restore-acknowledged"

// restoreDefaultReason is the reason of the event when the operator wrote none.
const restoreDefaultReason = "restore of the system database"

// onMismatch is what checkPublished does when the database is behind the record or contradicts it: look for a record of a later generation at the
// other holders (an acceptance made on another server), and otherwise either accept the restore (the operator wrote the confirmation) or refuse
// with the alert that names it.
func (c *Collector) onMismatch(ctx context.Context, snap ledgerstate.State, cause error, ownMsg string) error {
	p := c.cfg.Published
	if c.cfg.PullState != nil {
		if st, ok := c.cfg.PullState(ctx); ok && st.Generation > snap.Generation {
			if err := p.Merge(st); err == nil {
				return c.checkPublished(ctx)
			}
		}
	}
	msg := ownMsg
	db, err := c.buildState(ctx)
	if err == nil {
		conf := ledgerstate.Confirmation(snap, db)
		if given, reason := p.AcceptToken(); given == conf {
			return c.acceptRestore(ctx, snap, db, reason)
		}
		msg += fmt.Sprintf(" The database holds %d events and %d logs. To accept the loss and continue from the database, write %s in the file %s in the key directory of the manager leader (a second line may give the reason); a signed event restore-acknowledged then says that the history was cut. Read the section on restoring the system database first.",
			db.Events, len(db.Logs), conf, ledgerstate.AcceptFileName)
	}
	c.log.Error("the system database is older than what was published", "error", cause)
	c.publishedAlert(ctx, msg, true)
	return fmt.Errorf("%w: %v", ErrPublishedState, cause)
}

// acceptRestore makes the record what the restored database holds (one generation higher, the old record kept) and goes on.
func (c *Collector) acceptRestore(ctx context.Context, old, db ledgerstate.State, reason string) error {
	p := c.cfg.Published
	if reason == "" {
		reason = restoreDefaultReason
	}
	note := ledgerstate.RestoreNote{PublishedEvents: old.Events, PublishedLogs: len(old.Logs), PublishedSHA: ledgerstate.SumOf(old),
		DatabaseEvents: db.Events, DatabaseLogs: len(db.Logs), DatabaseSHA: ledgerstate.SumOf(db), Reason: reason, Actor: "operator, file " + ledgerstate.AcceptFileName}
	if err := p.Accept(db, note); err != nil {
		c.publishedAlert(ctx, "The restore could not be accepted: "+err.Error(), true)
		return fmt.Errorf("%w: %v", ErrPublishedState, err)
	}
	p.ConsumeAcceptToken()
	c.log.Warn("an operator accepted the restore of the system database: the record of what was published now holds what the database holds",
		"publishedEvents", old.Events, "publishedLogs", len(old.Logs), "databaseEvents", db.Events, "databaseLogs", len(db.Logs), "generation", old.Generation+1)
	c.publishedAlert(ctx, "", false)
	return nil
}

// announceRestore publishes the event of an accepted restore that is not announced yet, and ends the note. It is made at the start of every round
// that passed the check, so a crash or a failure between the acceptance and the event leaves it to the next round.
func (c *Collector) announceRestore(ctx context.Context) error {
	p := c.cfg.Published
	if p == nil {
		return nil
	}
	snap := p.Snapshot()
	n := snap.Restore
	if n == nil {
		return nil
	}
	fields := []ledgersign.EventField{
		{Name: "generation", Value: strconv.FormatInt(snap.Generation, 10)},
		{Name: "published-events", Value: strconv.FormatInt(n.PublishedEvents, 10)},
		{Name: "published-logs", Value: strconv.Itoa(n.PublishedLogs)},
		{Name: "published-state-sha256", Value: n.PublishedSHA},
		{Name: "database-events", Value: strconv.FormatInt(n.DatabaseEvents, 10)},
		{Name: "database-logs", Value: strconv.Itoa(n.DatabaseLogs)},
		{Name: "database-state-sha256", Value: n.DatabaseSHA},
		{Name: "reason", Value: n.Reason},
		{Name: "acknowledged-by", Value: n.Actor},
	}
	if _, err := c.cfg.Keys.AppendEvent(ctx, EventRestoreAcknowledged, "generation:"+strconv.FormatInt(snap.Generation, 10), fields); err != nil {
		return fmt.Errorf("publishing the event of the accepted restore: %w", err)
	}
	return p.ClearRestore()
}

// A chain that answers with another chain id than the one published for its group is closed only when the same new id has answered in every
// round for MissingFor (P7A-06): one wrong answer (a forged one, a mapping fault, a group that briefly answers from another database) must not
// end the publication of the real chain, which is irreversible and public. This is the shape of the rule for a database that vanished, and like it
// the clock is in memory: a restart or a change of leader starts it again, which is the safe direction.
type replacement struct {
	chainID []byte
	since   time.Time
}

func replaceKey(db string, g int32) string { return db + "|" + strconv.Itoa(int(g)) }

// replacementConfirmed notes that group g of db answered the chain id chainID, which is not the one of its log, and reports whether the same id
// has been answered without a single other answer for MissingFor. left is what remains of the time.
func (c *Collector) replacementConfirmed(db string, g int32, chainID []byte) (confirmed bool, left time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := replaceKey(db, g)
	r, ok := c.replacing[k]
	now := c.cfg.Clock()
	if !ok || !bytes.Equal(r.chainID, chainID) {
		c.replacing[k] = replacement{chainID: append([]byte(nil), chainID...), since: now}
		return false, c.cfg.MissingFor
	}
	left = c.cfg.MissingFor - now.Sub(r.since)
	return left <= 0, left
}

// replacementEnded forgets a pending replacement: the group answers the chain of its log again. It clears the alert that the first answer raised.
func (c *Collector) replacementEnded(ctx context.Context, db string, g int32) {
	c.mu.Lock()
	_, ok := c.replacing[replaceKey(db, g)]
	delete(c.replacing, replaceKey(db, g))
	c.mu.Unlock()
	if ok {
		c.alert(ctx, AlertDiverged, "critical", db, g, "", false)
	}
}

// replacementDone forgets a replacement that was carried out.
func (c *Collector) replacementDone(db string, g int32) {
	c.mu.Lock()
	delete(c.replacing, replaceKey(db, g))
	c.mu.Unlock()
}
