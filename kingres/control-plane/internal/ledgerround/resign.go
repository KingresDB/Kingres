// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"bytes"
	"context"
	"fmt"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
)

// Resign is the hook of a key rotation (ledgersign.Config.Resign, correction 47): the latest checkpoint of every log, growing or not, is
// signed again with the keys that sign now, so that a log that stops growing does not keep only the signature of the key that
// leaves. The note is verified under every key of the trust file first (ledgerlog.ResignCheckpoint), and only the signers that have no
// line in the note yet are added: a note that already carries all their lines is left as it is. A checkpoint of a closed or diverged log
// is signed again too: its note stays served for ever.
func (c *Collector) Resign(ctx context.Context, set ledgerkeys.Set) error {
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return err
	}
	if len(logs) == 0 {
		return nil // nothing is published yet
	}
	pc, err := c.prepare(ctx)
	if err != nil {
		return err
	}
	trusted, err := ledgerkeys.TrustEntries(set, ledgerkeys.TrustOptions{})
	if err != nil {
		return fmt.Errorf("the trust entries of the key set: %w", err)
	}
	for _, l := range logs {
		cp, err := c.cfg.Store.LatestCheckpoint(ctx, l.ID)
		if err != nil {
			if err == ledgerpub.ErrNotFound {
				continue
			}
			return err
		}
		n, err := ledgerlog.ParseNote(cp.Note)
		if err != nil {
			return fmt.Errorf("the stored checkpoint of %s: %w", l.Origin, err)
		}
		have := map[uint32]bool{}
		for _, s := range n.Signatures {
			if s.Name == l.Origin {
				have[s.ID] = true
			}
		}
		var more []ledgerkeys.Signer
		for _, s := range pc.Signers {
			if !have[ledgerkeys.KeyID(l.Origin, s.Public())] {
				more = append(more, s)
			}
		}
		if len(more) == 0 {
			continue
		}
		note, err := ledgerlog.ResignCheckpoint(ctx, cp.Note, trusted, more...)
		if err != nil {
			return fmt.Errorf("signing the checkpoint of %s again: %w", l.Origin, err)
		}
		if bytes.Equal(note, cp.Note) {
			continue
		}
		if err := c.cfg.Store.ReplaceNote(ctx, pc.Term, l.ID, cp.Size, note, pc.Revision); err != nil {
			return err
		}
	}
	return nil
}
