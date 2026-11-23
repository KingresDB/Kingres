<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0025: A restored system database is accepted by an operator, and the acceptance is a signed event

- Status: Accepted
- Date: 2026-11-23 (proposed 2026-11-20)
- Implemented in: `kingres/control-plane` (the ledger publisher and `accept-restore`, with the event `restore-acknowledged`); built and reviewed on the staging branch of the first release.
- Source: [../design/ledger-phase7.md](../design/ledger-phase7.md) (9.4 and correction 230), [../reviews/2026-11-20-kingresd-anchoring-wiring-and-publication-fixes.md](../reviews/2026-11-20-kingresd-anchoring-wiring-and-publication-fixes.md) (P7A-02)

## Context

The publisher of the ledger keeps what it has published outside the system database, in `published.json` in the key directory of every holder of the signing keys (the review P8-01). A system database that is restored from an older backup holds fewer checkpoints and events than were published, and the publisher refuses to sign while that is so. The first review of the fix (P7A-02) found that this refusal has no end: the database can only get back to what was published if something signs, and the refusal prevents that. The one way out was to remove `published.json` on every holder at the same time, which is the silent fork the record exists to prevent.

The design promised that the publisher re-derives the tiles and re-signs byte-identical checkpoints. That holds for the logs of the chains, because their leaves are the blocks and the chains are not in the system database. It does not hold for the aggregate log and the events: an aggregate leaf holds the clock time of its round and what the chains looked like then, and an event holds its time and the key set. Nothing computes them again.

## Decision

1. The refusal stays the default. It ends in two ways only: the database is brought back to what was published, or an operator accepts the loss.
2. The acceptance is a confirmation written in the file `accept-restore` in the key directory of the manager leader. The alert names the confirmation, which is sixteen hexadecimal digits of a hash of the record and of what the restored database holds, so it fits one situation and no other. The file may carry a reason on a second line.
3. The leader replaces its record by what the database holds, one generation higher, and keeps the old record as `published.json.before-restore-<generation>`. A record of a higher generation replaces the records of the other holders; one of a lower generation never takes the floor back.
4. The signed event `restore-acknowledged` is published in the first round after, before anything else is signed, with the generation, the counts and SHA-256 sums of the two states, the reason and the actor. A crash in between leaves a note in the record and the next round publishes the event.
5. The logs continue from the restored database. A mirror that holds the old checkpoints sees a fork next to the event that explains it.

## Consequences

- A cluster whose system database was lost can publish again. The price is public: the aggregate log and the events fork against what was published, and the event says so with the sums that identify the old record.
- The acceptance needs access to the host of the leader, as removing the record did, and nothing else. No REST operation exists for it, so the contract is unchanged. A console action can be added later on the same code.
- A restore that is accepted for a database that was not really restored (an operator who writes the confirmation to silence the alert) is a statement in the event of the publication, with the reason the operator gave.
- The text of design 9.4 that promised byte-identical re-signing is corrected to chain logs only.

## Alternatives rejected

- **Removing `published.json` on every holder.** No record, no trace of the break, and nothing tells an auditor that the history was cut.
- **A REST operation with a typed confirmation, like the acknowledgement of a diverged chain.** The publisher distrusts the system database that such a call would be authorised against. It also changes the contract and the console for a case that happens once.
- **Recovering the lost rows from a mirror.** The only way that keeps the history unbroken, and the way to prefer when a mirror exists, but it needs an importer that verifies tiles, checkpoints and events against the record. It is not built; the acceptance does not exclude it.
- **A new origin (epoch) for the aggregate log.** It changes the public format, which verifiers pin.
