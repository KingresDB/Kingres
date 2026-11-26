<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0015: Panel accounts are PostgreSQL roles

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Databases, users and servers in the panel" (Security page); Open decisions #10

## Context

The Security page in the document manages one list of accounts. Couchbase has its own user store with roles; CockroachDB and Patroni are cited for pages and APIs, not for user stores. The document lists four built-in cluster roles and says external identity providers (LDAP, OIDC) are out of scope for the first releases.

**[Recommendation]** in the document: panel accounts are PostgreSQL roles, and the panel signs a user in by authenticating against PostgreSQL. Cluster roles are memberships in `kingres_admin`, `kingres_ops`, `kingres_readonly` and `kingres_stats`. Roles and grants are propagated to every group through the same path as DDL. The audit log is append-only. No role, including `kingres_admin`, can turn the ledger off for a table.

## Decision

There is one account list. A panel account is a PostgreSQL role, authenticated by PostgreSQL, and its cluster permissions are its membership in the four built-in `kingres_*` roles. There is no separate panel user store.

## Consequences

- One list of users and one place for passwords; database users and panel users cannot drift apart.
- The panel's permission model is limited to what roles and memberships can express; panel-specific attributes need another place to be stored.
- Roles are cluster-wide state, so role changes must propagate to every group (the same path as DDL), including to the map group.
- **[Needs verification]** Propagation must carry the stored password verifier rather than the plaintext password, and the exact mechanism has not been checked. Another open point: PostgreSQL role objects are global per instance, so every instance in the cluster needs the roles in sync; the document does not describe how this works for roles created while a node is down.
- Sign-in depends on PostgreSQL being available. A node whose PostgreSQL instances are all down cannot authenticate panel users through them; "This server" pages are meant to work for a standalone or cut-off node, so a recovery path (for example a local administrator credential) needs a design. The document does not describe one.
- LDAP and OIDC are not supported in the first releases.
- Audit: every administrative action through panel, CLI or REST is logged to an append-only log.

## Alternatives considered

- **A separate panel user store**. Rejected in the document in favour of "one list of accounts, not two". No other argument is given.
- **External identity providers (LDAP, OIDC).** Out of scope for the first releases.
