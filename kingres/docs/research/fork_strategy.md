# Forking PostgreSQL into KingresDB: Upstream State, Mechanics, Legal, Rebranding, Prior Art, Extensibility, Builds

Research date: 2026-11-14. Items without a URL are not in this file as facts; they appear only under Gaps or Inferences.

## 1. Upstream state (Nov 2026) and which branch to base on

### Takeaway
PostgreSQL 18 (18.7) is the newest GA major as of 2026-11-14. PostgreSQL 19 is at RC2, with GA planned for 2026-11-19. PG14 reaches EOL on 2026-11-12, the date of its final minor release (14.25). A new fork started now should target PG18 stable, or PG19 stable immediately after GA, rather than `master`.

### Cited Findings
- Supported versions and current minors (page listing as of fetch): 18 (18.7, first release 2025-09-25, EOL 2030-11-14); 17 (17.12, EOL 2029-11-08); 16 (16.16, EOL 2028-11-09); 15 (15.20, EOL 2027-11-11); 14 (14.25, listed as supported, final release 2026-11-12). 13 and older are unsupported. — [PostgreSQL versioning policy](https://www.postgresql.org/support/versioning)
- Policy: one major per year; minor releases at least every 3 months; each major is supported 5 years, then a final minor release. Major upgrades need dump/restore or pg_upgrade; minor upgrades only swap binaries. — [Versioning policy](https://www.postgresql.org/support/versioning)
- PG19 RC1 was released 2026-10-15. RC2 was pulled forward to 2026-11-12, shipped alongside 18.7/17.12/16.16/15.20/14.25. Planned PG19 GA is 2026-11-19, unless a delay or extra RC is needed. RC1 notes list fixes in REPACK (CONCURRENTLY), pg_plan_advice, planner NULL-ability optimizations, logical replication, parallel autovacuum, and COPY JSON output. — [PostgreSQL 19 RC 1 announcement](https://www.postgresql.org/about/news/postgresql-19-rc-1-released-3409/)
- PG19 beta timeline: Beta1 2026-06-04, Beta2 2026-07-16, Beta4 2026-09-24 (from search snippets). — [Beta 2 news](https://postgresql.org/about/news/postgresql-19-beta-2-released-3350), [Beta 1 news](https://www.postgresql.org/about/news/postgresql-19-beta-1-released-3313/)
- Build systems: both autoconf (`configure`) and meson are still maintained. A PG19 Beta3 stamp commit touched `configure`, `configure.ac` and `meson.build`. Meson gets new options (e.g. static library toggle, Feb 2026) that autoconf lacks. — [Search results via PG19 commit mirror](https://forger.sitiv.fr/open-dsi/postgres/src/tag/REL_19_BETA1) (third-party mirror, secondary), [pgsql commit message](https://www.postgresql.org/message-id/E1vuYDi-000qV4-00%40gemulon.postgresql.org)
- Official clone URL is `https://git.postgresql.org/git/postgresql.git`. — [Docs: git](https://www.postgresql.org/docs/current/git.html)
- Meson docs (PG18) require Meson and Ninja, with Perl `IPC::Run` for TAP tests. Minimums: ICU 4.2, LLVM 14, libxml2 2.6.23, libcurl 7.61.0. — [Docs: meson build](https://www.postgresql.org/docs/current/install-meson.html)

### Inferences
- Branch choice: stable branches get only fixes and low-risk changes per the versioning policy, so basing on `REL_18_STABLE` (or `REL_19_STABLE` after GA) gives a stable base. Basing a product on `master` means tracking unreleased churn.
- Developing a sharding/ledger feature on PG19 gives the longest runway (EOL about 2031) if the team can wait for GA on 2026-11-19; PG18 is the safe default today.
- The release schedule makes a yearly "major rebase" cadence plausible.

### Gaps
- I did not verify the exact branch names (`master`, `REL_18_STABLE`, `REL_19_STABLE`) on github.com/postgres/postgres. The `REL_<n>_STABLE` pattern was only seen in a mirror URL. Run `git branch -r` after cloning.
- Whether `REL_19_STABLE` exists yet (normally branched around beta/RC) was not confirmed.
- Autoconf deprecation: no source found announcing deprecation. I could not retrieve the current autoconf install page.
- I did not retrieve the PG19 release notes (release-19.html), so I have no list of PG19 breaking changes.

## 2. Repo mirroring and fork mechanics

### Takeaway
The official repo is git.postgresql.org, mirrored to GitHub. Primary sources I could fetch do not describe private-branch strategies, so the merge-vs-rebase recommendations below are inferences from forks' reported experience.

### Cited Findings
- The PostgreSQL Developer FAQ says the main tree is hosted in git, and points to "Working with Git" and "Using Back Branches" wiki pages for branch/backport handling. It has no guidance on private branches or patch sets. — [Developer FAQ](https://wiki.postgresql.org/wiki/Developer_FAQ)
- Postgres-XL's maintainers reported that merging upstream master produced many more conflicts than expected, forcing a rebase onto a later commit first. A 2017 EDB post said a repo is "utterly broken" immediately after `git merge` from upstream. — [Working towards Postgres-XL 9.5](https://enterprisedb.com/blog/working-towards-postgres-xl-95), [Future of Postgres-XL](https://enterprisedb.com/index%2ephp/blog/future-postgres-xl)
- OrioleDB keeps its core changes as a published patch set (about 20 patches per a pganalyze overview), targeting a table AM API submitted upstream. — [pganalyze on table AMs](https://pganalyze.com/blog/5mins-postgres-bloat-table-access-methods), [Supabase acquires Oriole](https://supabase.com/blog/supabase-acquires-oriole)
- Babelfish ships "a patch against community PostgreSQL" per release plus extensions. — [babelfishpg.org source](https://babelfishpg.org/source/)
- Percona evaluated: maintain an existing patchset fork, write a new patchset, or an extension with minimal core changes; it chose minimal core patches in an easier-to-maintain fork. — [PGConf EU 2025 TDE slides](https://www.postgresql.eu/events/pgconfeu2025/sessions/session/7191/slides/807/PGConf%20EU%202025%20Riga%20TDE%20slides.pdf)

### Inferences
- Suggested mechanics, not sourced: keep `upstream` remote at postgres/postgres. Keep a pristine mirror branch per upstream stable branch (fast-forward only). Keep KingresDB changes on a separate branch, regularly rebased onto upstream minor tags (e.g. REL_18_x). Keep new code in separate directories (`contrib/`-style modules) to limit conflicts. Rebase patch series to isolate each core change as a small, documented commit.
- Rebase onto minor tags is cheap, since minors contain only fixes. Major-version moves are the expensive step (see section 5).

### Gaps
- No official guidance on GitHub-fork vs `git clone --mirror`. GitHub forks of large repos with disabled Actions/PR sync are a general practice that I did not source.
- No source on rerere, git-subtree or quilt-style patch queue practice.

## 3. License and trademark

### Takeaway
The PostgreSQL License permits any use, modification and redistribution if the copyright notice and license text are retained. The trademark policy (PGCAC) does not permit "Postgres"/"PostgreSQL" in a product, company or domain name without prior approval. The name "KingresDB" avoids the issue.

### Cited Findings
- License: use, copy, modify, distribute "for any purpose, without fee, and without a written agreement". The copyright notices (PGDG 1996-2026, Regents of the University of California 1994), permission paragraph and both disclaimer paragraphs must appear in all copies. No plan to change the license. — [PostgreSQL License](https://www.postgresql.org/about/licence/)
- Marks "Postgres", "PostgreSQL" and the Slonik logo are registered trademarks of the PostgreSQL Community Association of Canada. Policy last updated 2024-11-20. — [Trademark policy](https://www.postgresql.org/about/policies/trademarks)
- Fair use without approval: factually saying software "runs on, with, or is derived from PostgreSQL" (if true), articles, linking, identifying as a contributor, provided no affiliation or endorsement is stated or implied. — [Trademark policy](https://www.postgresql.org/about/policies/trademarks)
- Any other use (company name, product name, domain name or variants) needs permission "in our sole discretion". Refused if confusion is likely. You must not use the marks in a business/trade name or register any mark containing them. — [Trademark policy](https://www.postgresql.org/about/policies/trademarks)
- Prohibited uses include use "on competitive products, meaning databases that are not PostgreSQL, or that are built on or derived from PostgreSQL", use implying sponsorship, and modifying/combining the marks with other logos. — [Trademark policy](https://www.postgresql.org/about/policies/trademarks)
- Recommended attribution: "Postgres, PostgreSQL and the Slonik Logo are trademarks or registered trademarks of the PostgreSQL Community Association of Canada, and used with their permission." Approval requests: board@lists.postgres.ca. — [Trademark policy](https://www.postgresql.org/about/policies/trademarks)
- Pre-2020-12-08 uses are exempt if consistent. A 2021-22 dispute with a third party registering "Postgres" marks in the EU/US was reported by the project. — [Project news](https://www.postgresql.org/about/news/trademark-actions-against-the-postgresql-community-2302), [Setting the record straight](https://www.postgresql.org/about/news/setting-the-record-straight-more-updates-on-a-trademark-dispute-2682)

### Inferences
- Do not use the Slonik logo or "Postgres/PostgreSQL" in KingresDB's name, domains, or logo. Describe it factually as "derived from PostgreSQL" in documentation with the trademark attribution line.
- Keep `COPYRIGHT` file unchanged; add KingresDB copyright as an additional line.
- This is not legal advice. Get counsel review before launch, especially of the policy's "competitive products" clause, whose reach for derived databases is ambiguous (the same policy allows "derived from" as a factual statement).

### Gaps
- The official policy is ambiguous on how the "derived from PostgreSQL" fair use interacts with the ban on use "on" derived competitive products. No clarifying source found.
- No source on trademark approvals actually granted to derivative products.

## 4. Rebranding: what to change and what to keep

### Takeaway
I found no authoritative documentation listing rebrand points. The rebranding inventory below is not sourced and needs verification by grep in the actual tree.

### Cited Findings
- Babelfish demonstrates that behavior changes (wire protocol) can be added through hooks instead of renaming core: "protocol hooks" are described as the backbone of the product. — [Babelfish PostgreSQL hooks docs](https://www.babelfishpg.org/docs/internals/postgresql-hooks)
- Percona's pg_tde is documented as not a drop-in extension for community PostgreSQL. A modified server can still be required by extensions. — [Percona pg_tde coverage](https://www.percona.com/blog/the-making-of-an-open-source-postgresql-tde-extension/)
- `meson.build` and `configure.ac` both carry the version (the PG19 Beta3 stamp commit touched both). — [PG19 mirror](https://forger.sitiv.fr/open-dsi/postgres/src/tag/REL_19_BETA1)

### Inferences (unsourced engineering judgment, verify in code)
- Likely places to change: `PACKAGE_NAME`/`PACKAGE_STRING` in `configure.ac` and `project()` in `meson.build`, `version()` banner, `--version` output, installer/service names, docs titles.
- Keep unchanged for ecosystem compatibility: `PG_VERSION` major numbering semantics, `PG_VERSION_NUM`, `PG_MAJORVERSION`, `pg_config` output used by PGXS, `PG_MODULE_MAGIC` ABI check, binary names `postgres`/`psql`/`initdb`/`pg_dump` (rename only via optional wrapper aliases), port 5432 (or choose a different default knowingly), frontend/backend protocol v3 version, catalog version number discipline (bump `CATALOG_VERSION_NO` when catalogs change). Reason: drivers (libpq, JDBC, etc.) and extensions detect features by these.
- Expose KingresDB's own version via a separate GUC/function (e.g. `kingres_version()`) and append to `version()` rather than replacing the PostgreSQL version token, which drivers parse.

### Gaps
- No primary source found for the above (searched, not found). In particular: whether any driver parses `version()` / `server_version` strictly; how EDB Advanced Server, Greenplum or others brand `version()` and ports.

## 5. How existing forks and derivatives manage divergence

### Takeaway
Hard forks with deep executor/catalog changes lag by years (Greenplum on 9.4, Postgres-XL stopped around 9.5-10 era, YugabyteDB stayed on PG11 until PG15). The current pattern is minimal core patches plus extensions, with an aim to upstream hooks.

### Cited Findings
- Greenplum is based on PostgreSQL 9.4 per Cloudberry docs. Apache Cloudberry 2.x is built on PG 14; main is on PG 16. The 14.4 to 16.9 kernel upgrade pulled in more than 5,700 upstream commits; the next target is PG18. The project says such upgrades for MPP architectures "require substantial engineering effort and time". Greenplum itself merged PG 9.1 in 2018 with 19 developers. — [Cloudberry vs Greenplum](https://cloudberry.apache.org/docs/introduction/cbdb-vs-gp-features), [Cloudberry 2025 review](https://cloudberry.apache.org/blog/apache-cloudberry-2025-review.md), [Greenplum 9.1 merge](https://content.pivotal.io/greenplum-community/we-have-merged-postgresql-9-1-into-greenplum-database)
- Postgres-XL: 9.5 and 9.6 merges were harder than expected. A third-party archive notes no upstream activity since 2019-08-08; another third-party page says last release was September 2018. Sources conflict on exact dates; the second is low quality. Upstream direction discussed in hackers was FDW-based sharding instead. — [2ndQuadrant 9.5](https://www.2ndquadrant.com/en/?p=9251), [Future of Postgres-XL](https://enterprisedb.com/index%2ephp/blog/future-postgres-xl), [hackers thread](https://hackorum.dev/topics/34214)
- YugabyteDB reuses a fork of the PG query layer. It moved beyond PG11: v2.25 (preview) and v2025.1 (stable) are based on PG15, with an online upgrade where nodes run mixed PG11/PG15. Each change from main must be merged into and tested on the PG15 branch. MERGE not yet implemented. — [YugabyteDB moves beyond PG11](https://www.yugabyte.com/blog/yugabytedb-moves-beyond-postgresql-11/), [PG15 features](https://docs.yugabyte.com/stable/api/ysql/pg15-features/)
- OrioleDB: needs patches to core, mainly table AM API changes submitted upstream in 2023 (Korotkov) with reviewer pushback on API size. Maintainers said making it a pure extension will take more time. Supabase says it aims to minimize and upstream the changes. — [pgsql-hackers: Table AM Interface Enhancements](https://postgrespro.ru/list/id/CAPpHfdvi5env4eqQMeE0wkV7k6a5Rc8BPiYMt0H4yEqSP03-Bw@mail.gmail.com), [Beta12 thread](https://hn.jakechampion.name/item/44570389), [Supabase acquires Oriole](https://supabase.com/blog/supabase-acquires-oriole)
- Babelfish: patch against community PostgreSQL plus extensions (common, TDS, T-SQL), extensions cannot run on stock PostgreSQL; patches are published with every release. Pigsty lists babelfishpg_tsql 5.4.0 for PG17/18. — [babelfishpg.org](https://babelfishpg.org/source/), [Pigsty](https://pigsty.io/ext/e/babelfishpg_tsql/)
- Percona pg_tde: began as an extension, moved to a patched-core model with minimal core changes; pg_tde 2.2 is tied to Percona Server for PostgreSQL 17/18; its SMGR patch builds on Neon's earlier work. Percona says it aims for upstream acceptance. — [Percona blog](https://www.percona.com/blog/your-datas-not-safe-until-its-tde-safe-heres-how/), [PGConf EU 2025 slides](https://www.postgresql.eu/events/pgconfeu2025/sessions/session/7191/slides/807/PGConf%20EU%202025%20Riga%20TDE%20slides.pdf)
- Citus "unforked" from PostgreSQL by refactoring into an extension, reducing the work to track new PG versions. — [Percona search material citing Citus](https://www.percona.com/blog/your-datas-not-safe-until-its-tde-safe-heres-how/) (secondary; original Citus announcement not retrieved)

### Inferences
- Sharding done in the executor/transaction layer (Postgres-XL, Greenplum style) correlates with falling behind. Sharding via an extension (Citus) correlates with fast tracking of new PG majors.
- KingresDB should plan for a bounded, documented core patch set (like OrioleDB/Babelfish/Percona) and put sharding and ledger logic in extensions.

### Gaps
- Not researched or no usable result: EDB Postgres Advanced Server, openGauss, Neon repo practices, Postgres Pro, Amazon Aurora, TimescaleDB. No data on their current PG base versions.
- No engineering talk (PGConf.dev) specifically about long-term fork maintenance was found.
- Cloudberry sources disagree on current kernel version (14 vs 16); newest project material says 16 on main.

## 6. Extension points versus core patches

### Takeaway
PostgreSQL offers hooks, custom WAL resource managers, table AMs and `shared_preload_libraries`, enough for a lot of work. Only the custom WAL resource manager doc was confirmed in this research; the rest is not sourced here.

### Cited Findings
- Custom WAL resource managers: define an `RmgrData` struct and call `RegisterCustomRmgr` from `_PG_init`. IDs 128-255; use `RM_EXPERIMENTAL_ID` (128) while developing. The module must stay in `shared_preload_libraries` as long as custom WAL records may exist, or startup can fail. Typical users are table/index AMs needing recovery, replication or logical decoding. Reference module: `src/test/modules/test_custom_rmgrs`. — [Docs: Custom WAL Resource Managers (PG19 docs)](https://www.postgresql.org/docs/19/custom-rmgr.html)
- A third-party guide says custom rmgrs arrived in PG16 (not verified against release notes). — [WAL for Extensions](https://ikouchiha47.github.io/postgresql/04-wal/wal-for-extensions)
- Babelfish relies on hooks added in its patch (cache reset, parser, protocol hooks). — [Babelfish hooks](https://www.babelfishpg.org/docs/internals/postgresql-hooks)
- Table AM API gaps blocked OrioleDB from being a pure extension. — see section 5.

### Inferences
- A ledger or sharding feature needing its own storage and WAL could use table AM + custom rmgr, with gaps filled by small core patches.
- Dedicated sharding in an extension with FDW/custom scan/planner hooks mirrors the Citus approach.

### Gaps
- No sources fetched for: `planner_hook`, `ExecutorStart_hook`, `ProcessUtility_hook`, Custom Scan API, table AM callbacks, background workers, logical decoding output plugins. These exist in PostgreSQL docs (chapters on Table Access Method Interface, Custom Scan, Background Worker, Logical Decoding), but I did not retrieve them. Verify before use, including any hook signature changes in PG18/19 (e.g. `ExecutorStart_hook` behavior).
- Whether PG19 changed custom rmgr or table AM APIs: not checked.

## 7. Building and testing on Linux and Windows

### Takeaway
Use meson/ninja (official docs) on Linux. On Windows 11, MSVC (VS 2022) with meson is the better-supported route; WSL2 or Docker gives a Linux build with the least friction. MSYS2 had reported meson problems in 2023.

### Cited Findings
- Meson workflow: `meson setup build --prefix=...`, `meson configure -Dcassert=true`, `ninja` (or `meson compile`), `meson test` (as unprivileged user), `meson test --setup running` against a running server, `ninja install`. TAP tests need `-Dtap_tests` and Perl IPC::Run. Optional libs autodetected; `pkg-config` used. `--backend=vs` supports Visual Studio. — [Docs: meson](https://www.postgresql.org/docs/current/install-meson.html)
- The initial meson commit says Windows/MSVC postgres builds with ninja or msbuild. — [Meson commit mirror](https://forger.sitiv.fr/open-dsi/postgres/blame/commit/a1261cd16f07f6db12a54b94d9e36175198951b4/meson.build) (third-party mirror)
- 2023 pgsql-hackers: Andrew Dunstan reported Windows meson issues with MSYS2 only; MSVC meson build behaved well (may be outdated). — [pgsql-hackers: Windows meson build](https://www.postgrespro.com/list/id/3e9b9c1934d02d7236bc880515058415@postgrespro.ru)
- Old docs (PG14) recommend Visual Studio 2022 for Windows builds and say building from source is for developers. Third-party example: Visual Studio plus meson from the x64 Native Tools prompt. — [PG14 Windows docs](https://www.postgresql.org/docs/14/install-windows.html), [orafce README.meson_msvc](https://api.pgxn.org/src/orafce/orafce-4.14.6/README.meson_msvc)
- Developer FAQ recommends `--enable-cassert --enable-debug` builds and running `make check`, isolation tests, Valgrind, and the CI setup for cross-platform checks. — [Developer FAQ](https://wiki.postgresql.org/wiki/Developer_FAQ)

### Inferences
- Suggested CI (unsourced): GitHub Actions matrix with Linux `meson test` (and `-Dtap_tests=enabled -Dcassert=true`), Windows MSVC job, plus a nightly job that merges the newest upstream stable tag and runs the full suite. Upstream also has its own CI configuration in-tree (mentioned in the Developer FAQ); I did not verify its file layout.
- On the developer's Windows 11 machine, the lowest-risk path is WSL2 (Ubuntu) or Docker for daily work, with an MSVC build as a CI/compat check.

### Gaps
- The current official Windows install page returned 404 (the `install-windows.html` URL for current docs); current Windows build requirements are unverified for PG18/19.
- No source for MSYS2 status in 2026. No Docker build recipe researched.
- Autoconf `make check` details were not retrieved.
