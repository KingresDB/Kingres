<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0024: KingresDB's own code is licensed under the GNU Affero General Public License, version 3

- Status: Accepted
- Date: 2026-11-15
- Supersedes: [ADR 0004](0004-postgresql-license-no-citus-code.md), in its choice of licence. Its rule about not copying Citus code stays.
- Source: decision of the project owner, 2026-11-15; [../architecture.md](../architecture.md), "License and trademark" and open decision 3

## Context

ADR 0004 released KingresDB's own code under the PostgreSQL License, the licence of upstream. That licence is permissive: anyone may take the code, change it and distribute or operate the result without publishing their changes. ADR 0004 named the cost, that there is no copyleft protection for KingresDB's own code, and the project owner accepted it then.

On 2026-11-15 the project owner decided to change the licence to the GNU Affero General Public License, version 3. All of KingresDB's own code was written for this project and its copyright belongs to RooyeKhat Media Co, so the holder can change the licence of its own code. PostgreSQL's code is not KingresDB's to relicense.

## Decision

1. **KingresDB's own code is licensed under the GNU Affero General Public License, version 3 only** (SPDX identifier `AGPL-3.0-only`). That is everything under `kingres/` and `contrib/kingres_*`: the extensions, the control plane, the web console, the verifier, the packaging, the scripts, the tests and the documentation. "Version 3 only" means that a later version of the licence does not apply by itself; the copyright holder can add it later, which cannot be taken back, so the narrower choice is the one that keeps both options.
2. **PostgreSQL's code stays under the PostgreSQL License.** The file `COPYRIGHT` at the top of the repository is upstream's and is not changed, and every upstream file keeps its header. The small changes to upstream files that the core patches make ([../dev/core-patches.md](../dev/core-patches.md)) stay under the licence of the files they are in. The PostgreSQL License allows its code to be part of a work that is distributed under the AGPL, as long as its notices are kept; they are.
3. **A file of KingresDB that contains code adapted from PostgreSQL** says so in a comment, keeps the PostgreSQL copyright notice next to that code, and carries `AGPL-3.0-only AND PostgreSQL`.
4. **The text of the licence** is in `LICENSE` at the top of the repository and in `kingres/LICENSE`; `kingres/COPYRIGHT` says which licence covers which part. Every KingresDB source file starts with `SPDX-License-Identifier: AGPL-3.0-only` and the copyright line.
5. **What was published before this decision stays available under the licence it was published with.** The commits of the public repository up to the one that introduces this change were offered under the PostgreSQL License, and whoever obtained them then keeps those rights for those versions.
6. **No code of Citus or of another project is copied because of this change.** The AGPL would make Citus's licence compatible, but everything in KingresDB was written from concepts and published algorithms, and it stays that way: copying would bring another copyright holder into the code, and the holder would lose the freedom to decide about its licence alone. A deliberate import of third-party code needs its own decision.

## Consequences

- Whoever distributes KingresDB, changed or not, must pass on the source of KingresDB's code under the same licence. Whoever changes it and lets others use it over a network, for example as a hosted database service, must offer those users the source of the changed version (section 13 of the licence). This is the protection the permissive licence did not give.
- A company that wants to build KingresDB's code into a product without these obligations cannot do so under this licence. As long as RooyeKhat Media Co holds the whole copyright it can offer other terms to such a company; a contribution from outside would need an agreement that keeps this possible. No such agreement exists yet, and no outside contribution has been accepted.
- Applications that connect to KingresDB as a database, through the PostgreSQL protocol or the REST API, are not derived from it and are not affected.
- The extensions run inside the PostgreSQL server process. Distributing the server together with them, as the package and the image do, is distributing a combined work whose KingresDB parts are under the AGPL and whose PostgreSQL parts keep their licence; the package's copyright file states both.
- Dependencies must have licences that can be combined with AGPL-3.0: the ones in use (MIT, BSD, ISC, Apache-2.0, the PostgreSQL License) can. A new dependency still gets a licence check, now against this licence.
- The console is the place where users interact with the program over a network. It shows the copyright line with a link to the project's site on every page; it should also link to the source of the version that is running, which it does not yet.

## Needs verification

- Legal review of the change, of the wording of `kingres/COPYRIGHT`, and of the trademark question that ADR 0004 left open. None has been done.
- Whether the console's notice satisfies section 13 for an operator who changes the code: the link to the source of the running version is missing.

## Alternatives considered

- **Stay with the PostgreSQL License** (ADR 0004). No obligations for anyone, and no protection.
- **AGPL-3.0-or-later.** Lets a future version of the licence apply without a new decision. Rejected for now: "only" can be widened later by the copyright holder, "or later" cannot be narrowed.
- **GPL-3.0.** Does not cover use over a network, which is how a database is mostly used by others.
- **A source-available licence that forbids competing services** (the kind several database vendors moved to). Not an open-source licence; it was not asked for.
