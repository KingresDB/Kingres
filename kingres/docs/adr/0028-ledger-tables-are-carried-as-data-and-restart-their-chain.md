<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0028: Ledger tables are carried as data, and the imported ledger starts a new chain

- Status: Accepted
- Date: 2026-11-22
- Source: [../design/data-export-import.md](../design/data-export-import.md) ("Ledger tables"), `contrib/kingres_ledger/README.md` ("Dump, restore and replication", "Updatable tables", "Copies of the database")

Scope: this decision applies to the database export and import of phase 14c, which is built and ships in 0.1.0; this ADR is its design.

## Context

A ledger table is a user table with capture triggers whose rows are entered into a hash chain; its updatable form has a
history table. The extension has no supported way to load an existing ledger: its triggers refuse `UPDATE` and `DELETE`
of the chain's rows, a restored database has a new chain identity, and a plain `pg_dump` leaves capture triggers of tables
that are not registered, which the verifier names. A ledger table cannot be partitioned, so it is always a local table.
An export that is opened by a person and an import into a new database must say what happens to such a table, because
people read "ledger" as "tamper evident" and a copy is not the original chain.

## Decision

1. A ledger table is **exported** like any table: its current rows are data. Its capture triggers and its history table
   are removed from the schema section by the normaliser. The manifest records its ledger mode, its policy and the head of
   its chain, read without writing (no digest is generated).
2. The history rows of an updatable table are exported as a data entry for the record and are **not loaded**.
3. A ledger table is **imported** by loading its rows first and then calling `ledger_enable(table, mode)`, which enters
   every existing row as the first entries of a **new chain**. The ledger policy of the source (`required`) is set again after the load and the verification, and the report says which tables had it; time-partition policies come back disabled.
4. The import is **refused unless the request says `acknowledgeLedgerRestart: true`** when the archive has a ledger table;
   the refusal names the tables.
5. On the new database `ledger_verify()` verifies the new chain and returns no rows. It says nothing about the source's
   chain and the digests published for the source do not apply to the copy. The console, the import report and the audit
   event say that the imported ledger does not carry the proof of the past.
6. A table CSV import into an existing ledger table is allowed and goes through the capture trigger; a table with the
   policy `required` is refused up front.

## Consequences

- A database with ledger tables can be carried; nothing pretends the copy is the original chain.
- The audit event of the import records the acknowledgement; an auditor compares the manifest's head with a digest the
  source published to tie the copy to a point of the original.
- The history of updatable tables survives in the archive but not in the new database; a user who needs it loads it into a
  table of their own.

## Alternatives rejected

- **Refuse ledger tables in an archive.** Safe, and makes a database with one ledger table impossible to export.
- **Skip their data and carry only the structure.** Loses data silently and still needs the acknowledgement.
- **Load the chain's own tables from the archive.** The extension forbids it by design (it would make a forged history
  loadable), and a new chain identity is how it tells copies apart.
- **Enable the ledger before the load.** Every row of a large load would go through the capture trigger one by one,
  and a policy `required` would refuse the load. Entering the rows once, after, is faster and equal in effect.
