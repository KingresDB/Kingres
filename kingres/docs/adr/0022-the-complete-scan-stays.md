<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0022: The row-moving distributed scan stays as the path that answers every query

- Status: Accepted
- Date: 2026-11-15
- Source: [../architecture.md](../architecture.md) (phases 9 to 12), [design/shard-phase2b.md](../design/shard-phase2b.md)

## Context

Since phase 2a a `SELECT` that spans shard groups is planned by PostgreSQL as if all the data were on the originating node. Each distributed table is read by a "Kingres Distributed Scan": the local buckets, plus one cursor per other group. Only restrictions are sent to the other groups, and every restriction is applied again to the rows that come back. The rest of the statement (joins, aggregates, sorts, window functions, subqueries) runs on the originating node, in PostgreSQL's own executor.

This path is slow for large tables, because it moves rows. It is also the reason that every query shape gives the right answer: there is no list of supported statements, and nothing is refused because the planner of the extension does not know it.

Phases 2b and 12 add faster paths: pushdown of aggregates, sorts, limits and colocated joins, repartitioned joins, approximate aggregates, columnar storage. Each of them works only for the statements that meet its conditions. Systems that have only such paths answer the remaining statements with "not supported".

## Decision

The row-moving distributed scan is permanent. In every version:

1. A `SELECT` that no faster path accepts is answered by the distributed scan, with the same result as before the faster paths existed. A faster path is an optimization that the planner may choose; it is never the only way to run a statement that the scan can run.
2. A faster path that cannot prove its conditions for a statement (shippability of an expression, colocation, agreement of the nodes on a collation or a function) does not raise an error. It leaves the statement to the scan.
3. Every faster path can be switched off by a setting, and with all of them off the extension behaves as in phase 2a. The tests of a faster path include a comparison of its answers with the answers of the scan for the same statements.
4. A new table kind (a table of a distributed schema, a columnar table, a time-partitioned distributed table) is readable through the scan before any pushdown for it exists.

## Consequences

- No query that works today stops working when a later phase lands, and a wrong decision of a pushdown rule shows as a difference from the scan in a test instead of as a wrong answer that has nothing to be compared with.
- The scan has to be maintained for every new table kind and every change of the catalog. This is permanent cost.
- A statement that falls back to the scan can be far slower than the same statement one release earlier or later with pushdown. `EXPLAIN` shows which path a statement took, and the documentation of each faster path lists what makes a statement fall back.
- The rule covers reads. Writes that span groups are still refused where no correct way to run them exists yet (see the limitations of each phase); the row router of phase 2b removes most of these.

## Alternatives considered

- **Remove the scan once pushdown covers the common cases.** Less code to maintain, but every statement outside the supported list becomes an error, which is the complaint users have about systems built that way.
- **Keep the scan only behind a setting that is off by default.** Protects users from slow statements they did not expect. Rejected: a slow correct answer with a visible plan is a better default than an error, and a `statement_timeout` already bounds the cost for those who want a bound.
