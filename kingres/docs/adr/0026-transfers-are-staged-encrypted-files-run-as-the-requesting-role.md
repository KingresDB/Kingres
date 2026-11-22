<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0026: Exports are staged encrypted files, and transfers run as the role that asked

- Status: Proposed
- Date: 2026-11-22
- Source: [../design/data-export-import.md](../design/data-export-import.md) (decisions 1, 2, 4 and 9, "Roles, privileges and audit", "What may run")

## Context

The console is to export a table to CSV, import a CSV into a table, export a database to one archive and import such an
archive into a new database. Four facts of the code decide how:

- A request that a follower forwards to the leader is read whole and answered into a buffer of at most 32 MiB, and the
  public server ends a request after 30 s of reading and 60 s of writing (`httpapi/forward.go`, `config.go`). A task
  that is also the HTTP response, or an upload that is one request, cannot work for the sizes in question.
- A `COPY` of a distributed table that fails with `KS001` (a bucket moved, a group changed its primary) after the first
  byte was sent to a browser cannot be repeated for that browser.
- The contract has four cluster roles on a strict ladder and every operation names one; PostgreSQL privileges are
  what bounds the data a role may read. A connection as the signed-in role exists only on a server where that role signed in
  (`cluster/proxy.go`).
- The SQL of an archive is text that somebody else may have written, and the import runs it.

## Decision

1. **An export is a task that writes a staged file; the browser downloads the file.** The file lives on the leader, is
   sealed segment by segment with AES-256-GCM under a per-file key wrapped by a master key outside the staging
   directory, has a retention time (24 hours by default), a quota and a free-space reserve, and is served with
   `Range` through a streaming relay from any server. The audit event carries the final size and SHA-256.
2. **Who may.** Every transfer operation needs `kingres_admin`, **and** the cluster setting `dataTransfer.enabled`
   (off by default), **and** the PostgreSQL privileges of the signed-in role, because data is read and written on a
   connection of that role. A staged file can be listed, read and downloaded only by the role that made it; any
   `kingres_admin` may delete it. No fifth cluster role is added.
3. **An import runs the archive's SQL as the importing role, never as the service role**, after the same refusal of
   dangerous roles that the workbench applies, statement by statement, after the whole plan was lexed and checked
   against an allowlist, on a connection to the new database that the import created and marked. Distribution, ledger
   and time-partition policies are recreated from a structured catalog document with the service role, never from SQL
   in the archive. Code (functions, triggers) is refused unless the request asks for it and says so in words.
4. **A leader change ends a transfer** (`transfer_leader_changed`, retryable) and nothing is adopted by a new leader;
   a failed database import drops only the database that carries its own marker.

## Consequences

- An export needs disk on the leader and a janitor; the limits and the codes are part of the contract.
- A role that is a superuser cannot import a database through the console (the workbench's rule). It can export.
- An administrator can allow transfers for a cluster, and a colleague with the same cluster role cannot take what the
  first one exported.
- The relay is new internal protocol (a new call; the internal version stays 1) and is used by uploads and downloads only.
- The archive's SQL cannot do more than a non-superuser can do in a database it owns, and most of what it could try is
  refused before the database exists.

## Alternatives rejected

- **Stream the table straight to the browser from the `COPY`.** Simplest, no disk. It cannot retry, cannot report the
  checksum before the data leaves, ends after 60 s of writing without work in the server, and a follower would have to
  buffer it. It remains a possible later mode for the command line client.
- **A new cluster role (`kingres_exporter`).** A finer permission, but it changes the ladder, the contract's role table,
  the generated types and `setRoleClusterRoles`, for a decision the three guards above already make.
- **Run the archive's SQL as the service role after filtering.** Needed for nothing: the structured parts are made from
  validated data, and the rest is what a non-superuser may do. A filter that has a hole would then be a superuser hole.
- **Run it as a dedicated import role.** It would need to exist on every cluster and be mapped for the proxy; the
  signed-in role with the dangerous-role refusal gives the same bound with the privileges the user already has.
- **Encrypt only with the volume's encryption.** Does not cover a copied staging directory or deleted blocks; costs
  nothing to add per file with the standard library.
- **Resume a transfer on a new leader.** Needs the files, the keys and the role connection to move; the cost is large
  and the benefit is a transfer that is cheap to repeat.
