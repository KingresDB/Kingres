<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Work log

The dated record of what was done on KingresDB, how it was verified, and what
is still open. Newest entry first. The required content of an entry is defined
in [dev/documentation.md](dev/documentation.md).

---

## 2026-11-28 (branch p8-design): phase 8 is designed

### What was done

- Wrote [design/phase8-global-snapshot.md](design/phase8-global-snapshot.md) (hybrid-clock commit timestamps, snapshots rewritten from a timestamp, two small core hooks, a model and a spike with a decision gate, the fallback of atomic visibility) and [design/phase8-multinode-ledger.md](design/phase8-multinode-ledger.md) (witness quorum first, a CometBFT notary of checkpoints second, no Raft, no replicated SQL, the consistent cut).
- Proposed ADRs 0030 to 0034 and added them to the index; updated the phase 8 paragraphs of architecture.md, the phase table of the README, the limitations guide and the changelog.
- Second review round (re-review of 344375cc7f: 1 critical, 3 high, 6 medium, 3 low new): the snapshot hook takes its own snapshot after the clock is raised, the token is observed before every group transaction, the clock limit is carried in WAL by a custom resource manager and the promotion stall is outside the commit hook, bounded recency has its own skew bound, the keeper is on demand with a 5 s hold. Each is in section 16.1 of the snapshot design.
- Revised after an independent review (4 critical, 8 high, 9 medium, 4 low; each answered by id in section 16 of the snapshot design and section 17 of the ledger design): the origin joins the prepare protocol, publish-then-draw covers the prepare timestamp, the sample clock is read after the snapshot, the snapshot hook moved to `snapmgr.c` on an extension-owned copy, KP-0007 dropped (`ProcArrayInstallRestoredXmin` exists in both trees), a model and differential tests added to the gate, witness quorums must intersect, epochs are cosigned and counted.
- Read in the code and recorded in the design: the 2PC and snapshot behaviour of `node_conn.c` and `recovery.c`, the commit timestamp code of PG18 and PG19 (`xact.c`, `twophase.c`; the PG19 rename of the origin timestamp), the absence of a snapshot hook, the monotonic-only clock rule of `kingresd`.

### How it was verified

- Documentation only; relative links checked by script.

### Still open

- The owner's decisions listed in section 14 of the snapshot design and section 13 of the ledger design.
- The spike (P8-S) is the first test of the technique; nothing is built.

## 2026-11-28 (branch kingres-18): the first release, 0.1.0, is published

### What was done

- **Release.** KingresDB 0.1.0 was published on two GitHub releases, one per PostgreSQL major: `kingres-0.1.0-pg18` (the tag is on `kingres-18` at `dc2b7f1047`) and `kingres-0.1.0-pg19` (on `kingres-19` at `d34c6c1026`). Each page carries the Debian package `kingres_0.1.0+pg<N>-1_amd64.deb`, the image archive `kingres-0.1.0-pg<N>-image.tar.gz` (load with `docker load`) and `SHA256SUMS`, whose lines name `pg18/...` or `pg19/...` paths (check with `sha256sum -c --ignore-missing`). The page of PostgreSQL 18 also carries the four binaries of the verifier (linux-amd64, linux-arm64, darwin-arm64, windows-amd64), `RELEASE`, and the signed sums of the verifier as `kingres-verify-SHA256SUMS` and `kingres-verify-SHA256SUMS.note`. The product `SHA256SUMS` of 0.1.0 is not signed; the project owner decided on this day that it is signed from the next release on.
- **The last two fixes before the tag.**
  - A bootstrap of the map group that is given up while it asks for the fence or holder lease lets go of the lease it adopted after the abandon (`69a1bb9685`). The abandon marked the journal and released the lease, and a call of the runner that was already on its way adopted the lease again, so the old leader whose primary was started again never won it and the drain never ended (found as an intermittent timeout of the cluster test of a given-up switchover of the map group). The wait for the fence lease looks at the journal before every call.
  - The abandon waits for the lifecycle lock of the standby agent for at most 25 seconds and then answers 503 with Retry-After (`6a82753964`, bound corrected in `dc2b7f1047`), instead of waiting until a create, a remove or a rebuild of a standby on that server ended. The same holds when a server forwards the abandon to the others: one that could not give its bootstrap up now makes the answer 503 with Retry-After, and a server where the feature does not run is still only one that did not answer. The 25 seconds are under the 30 seconds of the forwarding call and the write timeout of the HTTP server.
- **Documentation.** The README of the project, the README of `kingres`, the user guide (download, check and install a release; the limitations) and the wiki say that 0.1.0 is released and link to the two release pages.

### How it was verified

The documentation was read against the CHANGELOG, the packaging README and the release pages as the project owner described them; the links of the changed files resolve. No code was changed in this entry and no test was run for it. The tests of the two fixes are named in their commits.

### Needs-verification items

None.

### Decisions and deviations

- The product `SHA256SUMS` of 0.1.0 is not signed; from the next release on it is signed (decided 2026-11-28; the release script is changed separately).

### Still open

- Phase 15 (management of third-party extensions) is built in a branch and is being merged; it is not part of 0.1.0. The real extensions (pgvector, pg_cron, pgaudit) are not packaged yet.
- Phase 8 is future roadmap.
- Failover is new in 0.1.0: the limits are in [guide/limitations.md](guide/limitations.md).

### Core patches

None.

---

## 2026-11-24 (branch rel-prep, from g2): the documentation of the first release brought up to g2

### What was done

- **CHANGELOG.** The database export and import entries moved from `[Unreleased]` into `[0.1.0]` (g2 holds everything planned for the first release); new entries for the security hardening of the extensions and the alert `extensionSkipped`, the split locks, drain step 4 and the standby lag rule, time partitioning of distributed tables (`kingres_timepart` 0.3), `pg_stat_statements` and `plpgsql`, the hard failover of the map group with `lostServerId`, the escrow of the CA key and the switchover fix; the contract is 0.30.0. The Pending part is rewritten item by item (done, still open, dropped); the heading stays for the release step to remove.
- **Operator guide.** Section 6 (escrow, hard failover of the map group, the verdict, the lost candidate, the second standby), section 4 (drain hand-over, standby lag), section 10 (`extensionSkipped`), section 11 (distributed tables), new sections 13 (export and import of a database) and 14 (`pg_stat_statements`, PL/pgSQL). The guide README and the limitations follow.
- **README.** The phase table follows g2. Nothing is claimed as released.

### How it was verified

Every statement was read against `kingres/api/openapi.yaml`, the design notes and the work log of g2; the relative links and anchors of the changed files resolve (a script, run on Windows). No code was changed and no test was run.

### Still open

See the Pending part of the CHANGELOG. Two points of the guide are from the text of the design and not from a run: what exactly repeats the apply of an extension after `extensionSkipped`, and the list of predefined roles that make an importer dangerous.

---

## 2026-11-24 (branch p11-cp, from g2): the distributed schemas in the rebalance and the drain

### What was done

What the code showed first: the extension already plans a distributed schema as an item of `plan_rebalance` (the item `(colocation_id, 0)` of its schema group; a pinned schema is left out, 9.9) and `move_bucket` dispatches to the schema mover, so the control plane planned and ran schema items as buckets without knowing it. What was missing: the item had no name or kind anywhere (plan, status, report), the run called `move_bucket` and not the extension's schema move, and a drain of a server with a schema that cannot move (a ledger table, a table that is not logged, a table with no key and no full replica identity) started, did not empty the server, and failed after the 30 minutes of its wait.

- `rebalance`: `Move.Schema` (the name, read in `PG.Plan` by joining the plan with `kingres.dist_schema`; empty for a bucket), `Move.Kind()`. `RunMove` calls `kingres.move_schema(quote_ident(name)::regnamespace, target)` for a schema item, inside the same guard, fence and gate (`withShard` and `extgate`) as `move_bucket`; retries, skips, the audit and the alerts are the loop's and are unchanged. Migration `0044_rebalance_schema_move.sql` adds `rebalance_move.schema_name`, kept by `StartMove` and `AddSkipped` and read by `ScanMove`, so the status and the report name the schema after a restart or a change of leader.
- The drain: `Service.schemasOn` reads `kingres.schemas_on_group` of every group whose primary runs on the server (extension 0.9, in every trusted database, through `withShard`). `DrainPreview` lists a schema that cannot move as a blocker (`schema_pinned` for a ledger pin, `unprocessable` for the other reasons, each naming the schema and the database) and `Drain` refuses with the first of them before a task exists. `bucketsOn` already counted the one bucket of a schema group (and a move in `cleaning`); the comment says so and the wait of the end step names how many of the remaining buckets are schemas.
- Contract 0.31.0 (placeholder; additive only): `kind` and `schemaName` on the result of one move of `getRebalanceReport` (`PlannedMove` and `BucketMove` had them since 0.15.0 and are now filled); the descriptions of `drainServer` and `previewServerDrain`. `check-contract.mjs`, the README changelog, `schema.d.ts` and `ContractRevision` follow. Console: the report dialog shows a schema as `Schema <name>` like the plan and the progress do, the drain dialog says that schemas move and keys its blockers by code and text.

### How it was verified

Windows only (a full gate runs in WSL): `go build ./...`, `go vet ./...`, `go test ./...` in `kingres/control-plane` (the database tests skip), `node scripts/check-contract.mjs`, `npm run api:check`, typecheck, lint and vitest of `kingres/web`. Written and not run: the database tests of `internal/rebalance` (`schema_items_pg_test.go`, on a stand-in catalog recorded by `trustStub`) and the cluster test `TestDrainOfAServerThatHostsADistributedSchema` (`internal/clustertest/schemas_drain_test.go`: the preview and the refusal by a schema without a replica identity, a drain that moves two schemas and the buckets and ends empty, a rebalance by disk size that moves a schema).

### Open

- The tests that were written and not run are the list in the report of the branch; they run in WSL after the timing gate.
- A schema that is only pinned by a ledger table keeps the drain refused until the operator decides about that data (design 9.9).
- The drain does not offer `setReplicaIdentityFull` for the schemas; the operator sets the identity (the preview says which table).

---

## 2026-11-24 (branch p9-rel, from g2): distribute with related tables and `Table.operation` (phase 9, last two items)

Branch `p9-rel` from `g2` (`771af7587d`). Contract **0.32.0** (a placeholder; 0.31.0 is taken by another branch): 1 operation added (178 operations, 142 paths), additive only.

### What was done

- **(A) Distribute with related tables.** Design choice: a control-plane task and an option of the existing operation, as `shard-phase9.md` says ("it is a control-plane feature because it is a sequence of ordinary statements"; decision 11), not a new endpoint and not a function of the extension. The extension offers `internal_fk_component` (the component of keys over tables that are already in `kingres.dist_table`, used by undistribute and alter), which cannot see ordinary tables, so the closure over ordinary tables is read from `pg_constraint` by the control plane (`Graph`, behind the extension guard for `kingres.dist_table`), and `create_distributed_table(..., colocate_with)` is what puts the related tables in the group of the table. `listRelatedTables` (new, read-only) is the plan; `previewTableChange` and `distributeTable` take `relatedTables`. The plan is a pure function (`planRelated`: breadth first order, the column of each table derived through the keys, blockers `related_column_unknown`, `related_table_missing`, `related_table_unrelated`, `related_table_busy`; warning `related_table_distributed`). The task has one step per table and resumes at the first step that is not done (`kingres.dist_table` and `pg_constraint` are looked at). The keys are captured when the request is made and kept in the input of the task.
- **(B) The operation indicator.** `Table.operation` (nullable `TableOperation`: kind, role, source, taskId, state, startedAt, percent, step) in `listTables` and `getTable`, from the active tasks with a table subject (and the related tables of a distribute) and from `kingres.table_operation(_item)` through `extUsable`. Both reads became leader-bound (the tasks are in the leader's system database).
- **Console.** Related tables in the Distribute dialog (the list, the columns, the keys, the warning that no key is enforced meanwhile, online off), `TableOperationBadge` on the rows of the list and a line "Running now" on the table page, the change buttons off with the reason from the operation. The mock has the plan, the related distribute and `operation`.
- **Docs:** operator guide (section 5), limitations, control-plane README, CHANGELOG (the Pending item is gone), `api/README.md` changelog 0.32.0, `check-contract.mjs`.

### How it was verified

Windows only (a timing-sensitive gate runs in WSL; no WSL test was run): `go build`, `go vet`, `go test ./...` in `kingres/control-plane`, `node api/scripts/check-contract.mjs`, `npm run api:check`, `typecheck`, `lint`, `vitest run` and the new `tests/tables-related-0-32.test.ts` in `kingres/web`. The database tests (`ops/tablerelated_pg_test.go`) skip on Windows.

### Open

- Tests to run in WSL: see the report of the branch (`TestRelated*`, `TestTableOperations*` of `internal/ops` with `KINGRES_PG_BIN`; a cluster test `TestTableLifecycleRelated` of `internal/clustertest`; the browser tests).
- A failed related task has no retry: the tables that are still ordinary are distributed with a new request. A distribute with related tables whose tables are in several schemas of other servers' instances is not covered.

---

## 2026-11-24: the integration branch g2 (nine branches merged onto g16s)

Branch `g2`, from g16s (`cc5008adb2`), nine merge commits in this order: `xcat-2`, `split-fix`, `pgss`, `p12-dist-tp`, `ca-escrow`, `p10-drain4`, `p5-hard`, `p14b`, `p14c`.

### What was done

- **Contract numbers** (the placeholders of the branches are replaced; the next number in the order of the merges): 0.25.0 `p12-dist-tp` (descriptions only), 0.26.0 `ca-escrow` (3 operations), 0.27.0 `p10-drain4` (no operation), 0.28.0 `p5-hard` (no operation),
  0.29.0 `p14b` (2 operations), 0.30.0 `p14c` (1 operation). The contract of g2 is **0.30.0, 177 operations, 141 paths** (g16s: 0.24.0, 171 and 136). `p12-dist-tp` had its contract text only in its work area; it is now in `openapi.yaml` and in the changelog.
- **Migration:** `0050_archive_validation.sql` of `p14b` is `0043_archive_validation.sql` (g16s ends at 0042); `p14c` has none.
- **Test numbers:** `contrib/kingres_shard/t/278_dist_timepart.pl` is `281_dist_timepart.pl` (278 was taken by `xcat-2`), `contrib/kingres_timepart/t/010_install_script_current.pl` is `012_install_script_current.pl` (010 and 011 are `xcat-2`'s). The console tests named after a placeholder number are renamed (`archive-*-0-29`, `import-*-0-30`, `map-hard-pause-0-28`).
- **`kingres_shard--0.8--0.9.sql`:** the three branches (`xcat-2`, `split-fix`, `p12-dist-tp`) append to its end; the appended parts are kept in that order. `internal_begin_move` is defined by `split-fix` and by `p12-dist-tp`; the merged function has the `updated_at` touch of the first and the hint of the second. `kingres_shard--0.9.sql` is regenerated by `mk-install-script.sh`.
  The upgrade script `kingres_timepart--0.2--0.3.sql` carries the schema check of `xcat-2` (as every script does), `kingres_timepart--0.3.sql` is regenerated, and `t/011_old_script_schema_check.pl` covers 0.3.
- **Trust check (extguard) for the code that came from the branches:** `cluster/globals.go` (the extensions of a copied database: Verify, then PreInstall), `ops/extensions.go` (Install and Installed: extguard for the extensions of KingresDB, a plain catalog read for `pg_stat_statements`, whose schema is public),
  `ops/tableprep.go` (the access methods of the leaves of a distributed table: `extUsable` before `kingres.partition_leaf_storage`), `ops/dbexport_facts.go` (the export's own check of the sharding extension is gone, extguard decides), `ops/dbimport_steps.go` and `ops/dbimport_service.go`
  (the import: `PreInstallExtension` before and `Require` after `CREATE EXTENSION kingres_shard`; `requireExtensions` uses the guard for the sharding extension too). `p10-drain4`: the lag is read on the system database `kingres_system` (hardened) with a `pg_catalog` statement; the exemption is written in the comment in `rebalance/pg.go`.
  `p5-hard` and `ca-escrow` read nothing of the schema kingres in a user database. Tests: `ops/gate_g2_test.go`, `rebalance/lagsql_test.go`.
- `ContractRevision` of the archives of the export is 0.30.0.

### How it was verified

Windows only (no WSL, a full gate runs there): `go build ./...`, `go vet ./...`, `go test ./...` in `kingres/control-plane` (the database tests skip), `node scripts/check-contract.mjs` (all rules hold), `npm run api:check`, `typecheck`, `lint` and `vitest run` in `kingres/web`. The C and SQL of the extensions were only checked for consistency (the generators give no diff, the test lists have no gap or duplicate); they are built and tested later.

### Open

- The branches' own WSL tests were not run on the merged tree.
- `contrib/kingres_ledger/t/040_columnar_refused.pl` is in no test list (it was not in g16s either).

---

## 2026-11-23 (branch pgss, from g16): pg_stat_statements and plpgsql in the first release

### What was done

- **Preload.** `instance.PreloadLibraries` is `kingres_shard,kingres_ledger,kingres_columnar,pg_stat_statements`. The library-exists check of columnar became a table (`optionalLibraries`): a library whose file is not in `pg_config --pkglibdir` is left out of the list. `kingres_columnar` stays sticky (marker file `columnar-preload`); `pg_stat_statements` is deliberately not sticky and has no marker: a missing file must never keep an instance from starting, and the statistics hold nothing to protect. `EnsureConfig` upgrades an existing instance as before: warning, managed file rewritten with every other setting kept, a reload of a running instance so that `shared_preload_libraries` is pending a restart, and the console's restart of pending instances puts standbys first.
- **Install in databases.** `postgres` and `kingres_system` get the extension when the cluster is created (`installStatStatements`); every database of `database.create` gets it through `installAnalytics`, on every server; a server that is added gets it with the databases of the leader (`globals/apply`). `ExtStatStatements` joined the closed list of the extension apply kinds. **Existing databases:** an explicit `CREATE EXTENSION IF NOT EXISTS pg_stat_statements` by the administrator, not a lazy install (nothing in `kingresd` uses the view, so there is no first use to hang it on, and a superuser DDL in a user's database at an unforeseen time is not worth a view); the guide text says so.
- **Configuration API.** `pg_stat_statements.max`, `.save`, `.track`, `.track_utility` and `.track_planning` are settable (cluster, server group, server; the three session ones also per database), not managed, not refused, not cluster-only; `shared_preload_libraries` stays managed and its reason names the library. A test pins the classification.
- **Packaging.** meson builds every contrib module and PL/pgSQL, and the deb and the image take the whole staged tree, so both modules were already shipped for both majors (nothing in `meson_opts` leaves them out). Added: `build.sh` stops when either is missing from the staged tree; `check_postgres_modules` in `test/lib.sh` (package, image and compose tests); `test/test-modules.sh --deb|--tree|--list`; the flow checks the new preload value, the view in `postgres` and a plpgsql function.
- **plpgsql.** In every database `kingresd` makes (checked on every server by the cluster test). `kingres_shard` does **not** propagate `CREATE FUNCTION`: the function is local to each server (the cluster test pins it, so a later change of that fact fails the test and the guide gets corrected); a plpgsql function in a filter, a read and a write against a distributed table works from every server. `t/013_dist_scan.pl`, `t/124_pushdown_closed.pl` and `t/057_scan_errors.pl` already cover plpgsql functions in distributed queries, so no new TAP was added.
- Guide text for the integrator (a section on the statistics, their settings and PL/pgSQL) was written as a separate file, outside the repository; the branch rel-docs2 was not touched.

### How it was verified

See the report of the run for the counts per major; the tests are `instance` (preload, the upgrade of an existing instance, the library that the installation lacks, not sticky), `ops` (`installAnalytics`), `pgconfig` (the classification), the cluster test `TestStatStatementsAndPlpgsql` (three servers, PostgreSQL 18 and 19, lane l11) and `test/test-modules.sh`.

### Still open

- The shell tests of the package and the image (`test-deb.sh`, `test-image.sh`, `test-compose.sh`) were changed but not run here (they need a built package and Docker); `test-modules.sh` was run on installed trees.
- No console page for `pg_stat_statements`; the view is SQL only.

## 2026-11-23 (second entry): the leftovers of p12-dist-tp and the findings of its review

Branch p12-dist-tp, on top of g16 (merged, `t/252` rerun).  Contract revision 0.25.0 (numbered when merged into g2; the text is in `api/openapi.yaml` and the changelog of `api/README.md`).

### What was done

- **g16 merged** into the branch (the search_path hardening and `t/252`); the branch had been made from the merge before it.
- **Stale claims.**  `begin_leaf_conversion` takes the pid of the driver (`holder_pid`, with its start time); `check_cluster()` reports `leaf_conversion_stale` (the session is gone, or the claim was not renewed for six hours);
  `cancel_table_operation` releases such a claim (and refuses one whose session lives); the next claim takes it over; the hints of the refusals that a claim causes name these actions (the C check of `assert_no_move`, `internal_begin_move`,
  `begin_leaf_conversion`).
- **`timepart__dist_convert`** takes the claim first, reads the leaves and checks the nodes inside the block that releases it on every error, ends the `CALL` that a failed or timed-out leaf left on its node before it releases the claim, and its leaf limit
  is at most 5 hours (the lease is 6).  `partition_leaf_storage` raises an error for a bucket whose group has no active primary.  A distributed action has no sizes (the sentinel `bytes_after = 0` is gone).  `object_in_use` is `retry_later` for a
  distributed table only.  The budget of a policy reads the configured limit (`reset_val`) and `partition_leaf_budget` has a fallback; the budget of an attach is tested; `table_change_report('create_partition' | 'attach_partition')` has the blocker `leaf_budget`.
- **kingresd** keeps `kingres_timepart` at the version of the package on every server of every database (the job `extension.update` of the leader, through the `extension.install` apply); `listTablePartitions` says `mixed` and `other` as `timepart__dist_kinds` does.
- **Console:** the Format filter has `Mixed`, with a badge, a mock switch (`?analytics=timeseries,mixed`) and a vitest.
- Tests in `t/278`: stale claims (a live and a gone holder, the lease), the 6-hour takeover and the renewal, the attach budget, a multibyte name, the way back to heap, readers (one statement at a time, always complete rows), `convert_leaf` by a role that is not a
  superuser, DDL on a server that is not the map group primary (the claim is a record of the map group primary and is not needed on the nodes: DDL and the partition functions reach them through it), an error between the claim and the first leaf, and a killed driver
  (reported, released by `cancel_table_operation`, the leaf that was running ends).  In `distpart_test.go`: no sizes in the actions, and `TestTimepartUpdatedOnEveryServer` (a database on 0.2 is updated on three servers, then converts).
- READMEs of the three components and section 11.7 of the design say what changed, and that a `statement_timeout` of a driver covers the whole partition.

### How it was verified

- PostgreSQL 18.7 and 19.0, private installs, lane l10, `-Werror`: `kingres_shard` TAP in full, 236 files (59,827 tests on 18, 59,790 on 19, with `t/046_two_majors` run separately with both installs: 30 more), regress 25 of 25; `t/252` and `t/278` twice more each (224 tests per round);
  `kingres_timepart` regress 17 of 17 and TAP 693 tests (10 files); the cluster tests `TestPartitionPolicyOfADistributedTable`, `TestTimepartUpdatedOnEveryServer`, `TestTableStorageAndPartitionPolicies`, `TestTableLifecycle`; `go test` of `ops`, `partsched`, `httpapi`, `kingresd`; the 215 vitest tests of the analytics console,
  `vue-tsc`, eslint and prettier.
- Mutants, killed: the claim not released on an error (T9: also `D` and `H` fail), the budget of an attach removed (S5), the holder never gone (S6), `cancel_table_operation` not releasing (S7), the budget of `create_partition` removed (S1);
  the job `extension.update` never run (Go, `TestTimepartUpdatedOnEveryServer`); the `Mixed` option removed (vitest).

### Decisions and deviations

- A reader that holds a transaction open on the table makes the swap of a leaf give way (`swap_lock_timeout`): it is the behaviour of the conversion of an ordinary table, and the next run goes on; the test of readers uses one statement at a time.
- The listing of the partitions reads every node on each page; it is not cached (a cache would show a finished conversion as half done).

### Still open

- A driver that is killed outright cannot end the `CALL` that it left on a node: the leaf ends by itself, and the claim is reported and released by `cancel_table_operation`.
- `openapi.yaml` and the generated contract files wait for the revision number.

### Core patches

none.

---

## 2026-11-23: time partition policies on distributed tables (phase 12, St-D1a and St-D1b)

Covers branch p12-dist-tp (from g16), design [analytics-phase12-storage.md](design/analytics-phase12-storage.md) section 11 (new 11.7 says what was built).  These were the last two items of phase 12.

### What was done

- **kingres_timepart 0.3** (`kingres_timepart--0.2--0.3.sql`; `kingres_timepart--0.3.sql` is generated by the new `mk-install-script.sh`, which `t/010` checks).  A user-partitioned distributed table (hash on top, range partitions below, `kingres_shard` 0.9) has a policy like any other table:
  `timepart__key_info` and `timepart__children` read its key and its partitions (rows of `kingres.dist_partition`, one logical object with a leaf per bucket) and the plan, the checks and the history work on them unchanged.
  A run creates a partition with `kingres.create_partition(..., if_not_exists => true)`, retires it with `drop_partition` or `detach_partition` (on every node, one two-phase transaction), and converts the leaves of an old one
  (`timepart__dist_convert`, `kingres.convert_leaf`).  A group that a bucket move or a conversion claims (`object_in_use`) is tried again inside the action, with a wait that doubles, and then recorded `failed` with
  `lock_timeout: retry_later`: the run is `partial` and comes back within five minutes, by the existing rule.  The names of generated partitions are cut to 57 bytes (the leaves append `_bNNNN`).  The leaf budget is
  checked by `timepart_set_policy` (steady state: retention, or one year, plus `create_ahead`, times the buckets; SQLSTATE 54000 with the numbers).  New: `timepart_dist_partitions`, `convert_distributed_partition`.
  Only a superuser drives a distributed table (the partition functions of `kingres_shard` need one); an owner who is not one gets `failed` actions with the reason.
- **Conversion of the leaves (St-D1b).**  One action converts the leaves of one logical partition.  The colocation group is claimed first, then each leaf that is not columnar is converted on its node by
  `CALL kingres.convert_leaf(bucket_partition, leaf, ...)` through `kingres.internal_node_exec` (a session of its own, so the procedure commits): the algorithm of `convert_partition` with the bucket partition as the
  table, `REPLICA IDENTITY FULL` on the new columnar leaf, and the swap under `kingres_shard.enable_routing = off` (requirement R4: no new authorization was needed).  A leaf that refuses leaves the partition mixed
  (reads are right, the next run goes on).  A converted bucket moves (St-S3), tested.
- **kingres_shard 0.9** (`kingres_shard--0.8--0.9.sql`, `kingres_shard--0.9.sql` regenerated): `create_partition` and `attach_partition` enforce `kingres_shard.max_partition_leaves` (before, only the distribution of a table did),
  `partition_leaf_budget()`, `partition_leaf_storage()` (the access method of every leaf, read on its node), and the claim of a colocation group for a conversion (requirement R5): `begin_leaf_conversion`,
  `renew_leaf_conversion`, `end_leaf_conversion`, a record of kind `convert_leaf` in `table_operation` (phase `done`) with a row of `table_operation_lock`, which the moves, the table operations, the partition functions and
  every DDL statement on a table of the group already read.
- **kingresd.**  The scheduler, `partition.maintain` and the policy operations needed no change of structure (the leader's instance holds the map group primary, so a distributed table is one of its tables).  Changed:
  the partition column and the health of a policy are read through `kingres_timepart` (they named the distribution column), a policy over the leaf budget is `partition_policy_conflict`, and `listTablePartitions` of a
  distributed table has `rangeFrom`, `rangeTo` and `accessMethod` (`heap`, `columnar`, `mixed`).  The contract text is in `contract.txt` of the work area (numbered 0.25.0 in g2); the console needs no change.
- READMEs of `kingres_shard`, `kingres_timepart` and the control plane; section 11.7 of the design.

### How it was verified

- PostgreSQL 18.7 and 19.0 (private installs, one source, `-Werror`): `kingres_timepart` regress 17 of 17 and TAP 10 files with 693 tests on each major (`upgrade` compares the upgrade from 0.1 and from 0.2 with a new install of 0.3; `t/010` the generated install script);
  `kingres_shard` regress 25 of 25 and the whole TAP suite, 236 files: 59812 tests on 18 and 59741 on 19, all pass (`t/046_two_majors` run with both installs, 30 tests; `t/250` holds for 0.8 and 0.9); the new `t/281_dist_timepart.pl` has 145 tests on each major
  (creation ahead, conversion with `REPLICA IDENTITY FULL`, retention, the leaf budget, the 57-byte names, a claimed group and a held bucket move in both directions, a leaf that refuses, a move of converted buckets, detach, the owner who is not a superuser).
- Control plane: `go test` of `internal/ops`, `internal/partsched`, `internal/api`, `internal/httpapi` and `internal/contract` pass; the cluster test `TestPartitionPolicyOfADistributedTable` (three servers, lane l10: policy, plan, a run by hand with three conversions and a drop, a busy group, the scheduler) passes on 18 and on 19;
  `TestTableStorageAndPartitionPolicies` and `TestTableLifecycle` (the listing of partitions and the policy of an ordinary table) pass on 18.
- Mutants (each applied alone to a copy of the extension, the real one reinstalled after each, `t/278` run): the leaf budget of `create_partition` removed (S1), the claim without its lock row (S2), `partition_leaf_storage` without
  the access method (S3), the claim without its busy check (S4), `object_in_use` not retried (T1), `REPLICA IDENTITY FULL` not set (T2), the policy without the budget check (T3), the plan that does not read the storage of the
  leaves (T4), the claim not released after a conversion (T5), the swap without `enable_routing = off` (T6), detach replaced by drop (T7), the 57-byte name replaced by 63 (T8); Go: the partition column from `pg_partitioned_table`
  (G1), the listing without the storage and the budget error without its mapping (G2, G3); all killed, each by the test that names its behaviour.

### Decisions and deviations

- The retry is the existing loop of the extension (`lock_retries`, a doubling wait) and the action is `failed` with `retry_later`, not the 5 s to `partition_retry_window` and `skipped` of the first sketch; the run's end rule (partial, five minutes) is the one that was there.
- The claim reuses `table_operation_lock` with a record of kind `convert_leaf` in phase `done`, instead of a lock of its own: every exclusion that exists already honours it, and the machinery that resumes unfinished table operations does not see it.
  The claim is not renewed by a heartbeat process: the conversion renews it after each leaf, and a claim of six hours without renewal is taken over.
- The leaf budget is `kingres_shard.max_partition_leaves` (8192), not the 20 000 of the first sketch of the design.

### Still open

- Closed by the leftovers entry of the same date below: the stale claim in `check_cluster()`, the update of `kingres_timepart` on every server, the Mixed filter and the leaf budget in `table_change_report`.
- The contract text is not in `api/openapi.yaml` (the revision number is assigned at the merge).

### Core patches

none.

## 2026-11-23: phase 10 closed in the control plane: the drain hands the map group over, and the automatic mode waits for a standby that lags

### What was done

Branch `p10-drain4` (from the staging branch g16). The two open items of phase 10 ([design section 23](design/shard-phase10.md)):

- **Drain step 4, the hand-over of the map group** (`internal/rebalance/handover.go`, `cmd/kingresd/drain_maphandover.go`). `drainServer` no longer refuses the server that runs the map group primary when another server runs a standby of it. A planned switchover is refused while a rebalance runs
  (migration 0009), so the new step `Hand over the map group` pauses the drain task, a goroutine asks for the planned switchover of group 0 (`failover.Service.Start`, graceful), and the job `RunDrainWatch` of the new manager leader resumes the drain when the switchover has ended (marker `drain.mapHandover`
  in `kingres_cp.cluster_setting`). A refused or rolled-back switchover fails the task with its code; the server stays draining. The reconcile of the standbys (step 5) leaves the former primary of group 0 in the ring until it is a ready standby (`cmd/kingresd/drain_standbys.go`):
  the first cluster run showed that a former primary that is still being rebuilt cannot be removed, and the step waited for the whole bound.
- **The standby-lag rule of the automatic mode** (`automatic.go`, `pg.go`). The rule "group without a healthy standby" also holds for a standby behind its primary by more than `maxStandbyLagBytes` (default 64 MiB), read on the primary of each group (`pg_stat_replication`); a primary that cannot be asked counts as lagging.
- Contract text only, revision **0.27.0**: `drainServer` description, `blockedBy` description, API README changelog, `check-contract.mjs`, the two texts in `web/src/api/schema.d.ts` (by hand).
- Documents: `control-plane/README.md` (drain, automatic mode, tests), `docs/design/shard-phase10.md` section 23, this entry.
- Commits: see `git log p10-drain4` (code and tests, contract, documents).

### How it was verified

- Unit tests (`go test ./internal/rebalance ./internal/ops ./internal/failover ./internal/contract`): `handover_test.go` (decision table, readiness of the marker, refusal of the map group server, the step with an engine over memory: pause, one request, resume, refusal code, rollback, a hook error recorded without its text, no request before the pause), `lag_test.go`.
- With PostgreSQL (lane l8, private installs, PostgreSQL 18 and 19.0): `handover_pg_test.go` (marker in the system database with the age by the clock of the database, the watch resumes a started or refused hand-over, waits for an unfinished failover, leaves a young marker, another task, a manual rebalance and a server that is not the leader alone, resumes a marker left behind), `lag_pg_test.go` (facts, the setting, an unreadable primary, no standbys wanted, the guard).
- Cluster tests (`internal/clustertest/phase10_drain4_test.go`, PostgreSQL 18 and 19.0): `TestDrainHandsOverTheMapGroup` (three servers, standbys, writers on another server through the whole drain of the manager leader: the drain ends done under the new leader, the step is done, group 0 is elsewhere, the drained server holds no bucket and no instance, the other servers see every row and every acknowledged write, the failover report is there),
  `TestDrainWhoseSwitchoverOfTheMapGroupIsGivenUpFailsAndLeavesTheServerDraining` (the candidate cannot promote, the bootstrap is given up: the drain fails with `server_hosts_map_group`, the old leader holds the map group again, the flag stays false, the rebalance lock is free), `TestAutomaticRebalanceWaitsForAStandbyThatLags` (replay paused on the standby of the group that owns the buckets, 8000 rows written, limit 1 MiB: blocked by `groupDegraded`, no run for 15 s, then the replay resumes and the run completes).
  The existing `TestDrainOnACluster` (its preview of the map group server now says it can be drained) and `TestAutomaticRebalanceOnACluster` pass on both majors.
- `node kingres/api/scripts/check-contract.mjs`: all rules hold (171 operations, 136 paths). Not run: `npm run api:check` of the console (no `node_modules` in the worktree), so the two hand-edited texts of `schema.d.ts` are unchecked against the generator.
- Mutants: 18 on the unit and PostgreSQL tests (lag comparison, unreadable lag, rule without the lag, rule without the replica count, a started marker that asks again, a step that does not pause, a request without waiting for the pause, a request twice, a marker that is ready at once, a refusal that ignores the standbys of the map group or the hook, a refusal that is not recorded, markers that are not cleared), all killed (two first versions did not compile and were redone); 2 at cluster level (the hook not wired: the preview and the drain refuse the map group server; the lag rule left out: no block), both killed. The wait for the former primary to be a ready standby was found by a failing cluster run (the drain stood in step 5 until the bound) and has no mutant of its own.

### Decisions and deviations

- The drain stays one task of kind `rebalance` (section 22); the hand-over pauses it instead of running the switchover inside the step, because the database excludes the two (see section 23).
- The lag rule is part of the automatic mode only; a manual start does not read the lag.
- The removal of the old leader after its drain was not tried in the cluster test: the first server holds the key of the certificate authority and `removeServer` answers `certificate_authority_unavailable` from another leader (an existing limit, unrelated to the drain).

### Fixes after the review of the branch (same day)

- M1: the marker has the states `requested`, `requesting` (written before the hook is called), `started` (after it, tried five times on new sessions) and `refused`; the audit event comes after the record of started; the watch also resumes `requested` or `requesting` when a failover created after the marker has ended (and turns the marker into started, so the resumed step fails if the server still runs the map group).
- L1, L3: the marker carries an attempt (a random name; ages are by the clock of the database, so no clock of a server is compared). The goroutine acts only while the marker carries its attempt; a young marker of the same task is not overwritten, and a manual resume of the paused drain pauses again.
- L2: the hook refuses with `failover_in_progress` while any failover task is unfinished, so `failover.Service.Start` never resumes an unrelated paused failover.
- L4: the marker is cleared when the step ends or fails, in `onFail` (failure and cancel, also while paused) and at the end of the drain (`Write the report`).
- L5: only `creating` and `rebuilding` standbys of group 0 on the drained server hold it in the ring; at the bound with `acceptFewerReplicas` the server is taken out of the ring anyway (hook argument `force`), and the task detail says that the drain ends with fewer standbys.
- M2: the lag query joins `pg_stat_replication` with the physical slots, reads only senders that are catching up or streaming, and answers -1 (lagging) for a standby with no replayed position; backup and logical walsenders are ignored; cascading standbys are not seen (documented).
- M3: inside a run the guard needs more than twice `maxStandbyLagBytes` on three evaluations in a row; the start and the resumption of a run use the limit; the groups are read in parallel under the same 15 s bound. Contract text, README and design section 23 say so.
- L6: the console mock lists the step `Hand over the map group`; the drain dialog says that the map group is handed over for the server that runs its primary. Vitest of the touched files, `npm run api:check`, eslint, prettier and `vue-tsc` pass (the `node_modules` of another worktree were linked).

Verification of the fixes: unit and PostgreSQL tests of `internal/rebalance` (whole package, PostgreSQL 18 and 19.0), `cmd/kingresd` (hook), console (vitest of the touched file and of the servers pages, `npm run api:check`, eslint, prettier, `vue-tsc`), the five cluster tests on both majors. 16 more mutants, all killed (a lagging row of one evaluation, no `requesting` record, no audit event, a young marker that asks again, the attempt ignored, readiness without the ended failover, the marker of any task cleared, no `force`, streak of one, factor of one, a hook that ignores an unfinished failover, sequential reads, a query without the physical filter, with backups, without the unreadable case, `onFail` that keeps the marker). One cluster run of `TestDrainHandsOverTheMapGroup` on PostgreSQL 19.0 ran into its 25 minute bound (the drain did not end under the new leader) and its log was not kept; six later runs on 19.0 and two on 18 passed. The test now prints the task, the rebalance status and the log of the new leader when it fails, so that the next occurrence can be read; the cause is not known.

Verification of the fixes: unit and PostgreSQL tests of `internal/rebalance` (whole package, PostgreSQL 18 and 19.0), `cmd/kingresd` (hook), console (vitest of the touched file and of the servers pages, `npm run api:check`, eslint, prettier, `vue-tsc`), the five cluster tests on both majors. 16 more mutants, all killed (a lagging row of one evaluation, no `requesting` record, no audit event, a young marker that asks again, the attempt ignored, readiness without the ended failover, the marker of any task cleared, no `force`, streak of one, factor of one, a hook that ignores an unfinished failover, sequential reads, a query without the physical filter, with backups, without the unreadable case, `onFail` that keeps the marker). One cluster run of `TestDrainHandsOverTheMapGroup` on PostgreSQL 19.0 ran into its 25 minute bound (the drain did not end under the new leader) and its log was not kept; six later runs on 19.0 and two on 18 passed. The test now prints the task, the rebalance status and the log of the new leader when it fails, so that the next occurrence can be read; the cause is not known.

### Closing items before gate2 (2026-11-24)

- The timeout of `TestDrainHandsOverTheMapGroup` on PostgreSQL 19.0: ten more runs on 19.0 (nine passed; the tenth stopped in the formation of the cluster, where the standbys were not streaming after three minutes, before any drain began, under the load of the gate) and five on 18 (all passed); a second formation stop on 19.0 hit the death test the same way and its rerun passed. The L5 suspicion was reasoned through: the five states of the CHECK, which are written, and how the former primary of group 0 passes through them (design section 23). It does not leave a state other than the ones that are waited for; the cause of the one timeout stays unknown.
- A manual start is refused with `group_degraded` while a standby lags by more than the limit, unless `RebalanceOptions.force` (contract 0.27.0, additive): `Service.manualLagGate`, unit and PostgreSQL tests, the cluster test `TestManualRebalanceRefusesAStandbyThatLagsUnlessForced`.
- A drain whose server dies while the map group is handed over: cluster test `TestDrainWhoseServerDiesWhileTheMapGroupIsHandedOver` (fault point `drain:handover` holds the request after the marker says `requesting`; kingresd and the instances are killed by pid; the map group is failed over by an operator). It found that step 5 waited its whole bound (30 minutes) for a dead server: step 5 now fails at once with `node_state_invalid` when the server is dead. The drain ends failed on the new leader, the marker is gone and the rebalance lock is free.
- The tests wait for the first rebalance to be idle before they start a drain (a report can exist before its task ends).

Mutants of the closing items, all killed: the manual gate (always allows, force ignored, a lag at the limit refused, an unreadable lag taken as none: the PostgreSQL tests of `internal/rebalance`), the dead-server check of step 5 (removed: the unit test waits out its bound and fails; a wrong code: the unit test; removed with the bound cut to 3 minutes: the cluster test, which now asserts that the drain failed with `node_state_invalid`), and the hold at `drain:handover` (never cleared: the unit test of `holdAtFault` hangs on its first call and is killed by the timeout). The two cluster tests of the drain of a dying server and of a given-up switchover were rerun on both majors on the final code; the cluster of these tests now waits up to ten minutes for its standbys (three formation stops on 19.0 on a machine with a load average of 14, before any drain began).

### Still open

- The cause of the one timeout of the cluster test on PostgreSQL 19.0 is unknown (it did not happen again in 14 runs); the test now prints what is needed to read it.
- The number 0.27.0 is the one the revision has in g2 (0.21.0 on the branch).

## 2026-11-23: phase 5 closing: the hard failover of the map group while its server is alive, and the hardening loops (increment 9)

Branch `p5-hard`, from the staging branch g16 (failover round 2 with the F7 fixes). Contract revision 0.28.0 (documentation of `failoverServer` only, see below).

### What was done

- **The last refusal of the failover is gone.** `failoverServer` with `mode: hard` for the leader's own server (the map group's primary answers) used to be 422 `unsupported_operation`. The design
  ([ha-phase5-part2.md](design/ha-phase5-part2.md), "A hard failover of a live leader's server", with the fencing argument) reuses the shape of the planned switchover of the map group: the map group is the
  last group of the task; its step `Stop the old primary` judges the loss before anything changes (a verdict the request did not accept pauses the task with the old leader running), starts the bootstrap of a
  standby's server (`planned.hard` with the leader's facts as of the stop), fences the leader's own primary in one call (`fencedStop`: the candidate is recorded, the renewal ends and the marker is written,
  an immediate stop, and the lease is handed over only after the stop is seen), and waits for the standby to be promoted. The candidate waits for the lease, ranks the standbys after the stop (the named
  target as for any group), promotes without a proof, marks the old leader's task with the pin and the verdict, and its engine finishes the task. A primary that does not stop is stopped by its watchdog when the
  unrenewed lease runs out, and the candidate takes the lease after that. A loss that shows only after the stop pauses the bootstrap, the pause names the server, and the repeated request goes there.
- **Two defects found while building it, fixed.** The bootstrap looked for the old holder among the grantors that refused (the keys of `RefusedBy`) and not among the holders they name, so a lease that was still
  renewed was reported as `lease_held_elsewhere` with no alert; and the term that the calls of a group expect was taken from the groups before it in the order of `Groups` and not in the order they run, which
  put the map group of a planned switchover with other groups on the server on a stale term.
- **Contract 0.28.0, documentation only:** the text of `failoverServer`; `info.version`, the changelog, `check-contract.mjs`, the README of the control plane and the web contract test follow.
  The contract log of this branch lists the files so that the merge can renumber.
- **Test faults** (`demote:nostop`, `demote:exit` for the fenced stop, `maphard:facts`, `pin:unreadable`), a sampler of the map group in the cluster tests that fails at two writable instances on two
  readings of one asker, and the lease, fence and stop lines of every server in the failure output of a switchover test.
- **Increment 9, hardening:** `run-failover-loops.ps1` now runs round by round with one build per round, a bound on the time (`-MaxMinutes`), a lane, and a summary per test read from the logs; the group
  `map-hard` was added. The README and `internal-api.md` of the control plane describe the new behaviour; ADR 0021 says what `HoldBack` and the fenced stop do to the lease.

### How it was verified

- Unit: 33 new tests (`maplive_test.go`, `mapgroup_hard_test.go`, `maphard_test.go`, the journal; the crash test has 16 subtests) and 5 cluster tests. **Mutants:** 31 on the unit tests (the order of start and fence, the loss check, the lease hand-over, the facts as of the
  stop, the context of the step, the order of the groups, the bound on the old server, the skips on the new leader, the preflight, the term, the lease wait, the proof, the pin, the repeated request, the
  journal fields, the transfer while the primary runs, the order of the fenced stop, the candidate record, the target, and others) and one on the cluster; all killed. Three first attempts did not count and were redone: one used a test
  name that did not exist, one was an equivalent mutation, one did not compile.
- Cluster, PostgreSQL 18.7 and 19.0 (private installs, lane l9): the five `TestHardFailoverOfTheMapGroup*` tests pass on both: the live leader's server (31 to 34 s from the request to the end of the task),
  the primary that does not stop (30 to 42 s: the watchdog and the lapse), the old leader's kingresd that ends after the fence (1 min 31 s to 1 min 34 s, the old leader away for 5 s), no readable standby
  (nothing changed), and the pause after the stop with the repeated request. The sampler saw no instant of two writable primaries (12 to 94 readings per test). **Cluster mutant:** the fenced stop without
  `Relinquish` fails `...WhoseOldPrimaryDoesNotStop` (the old leader keeps renewing, nothing is promoted: `promote_timeout`).
- **The loops** (`run-failover-loops.ps1`, one round of every group: 25 tests, 28 with subtests; the machine was shared with a full gate and other lanes, so the times are those of a loaded machine; the
  bound of two hours per major allowed one round and a few repeats): PostgreSQL 19.0: 28 of 28 passed, 113 minutes. PostgreSQL 18.7: 27 of 28 passed in 95 minutes; the failure is below. Times per test are in
  the table of the summary; the measured ones of section 2.5 of the design: an ordinary group, request to the end of the task after a kill, 45 s (18) and 40 s (19); the hard failover of the map group
  of a live leader's server 31 to 42 s; the planned switchover of the map group 129 to 208 s test time.
- **The one failure of the loops** (18.7): `TestSwitchoverSurvivesACleanStopThatOutlastsTheLease` timed out after 8 minutes because the leader's own system database (the map group's primary on the
  leader's server) was down and nothing could be read; four repeats of the test alone passed (about 3 minutes each) and the test passed in the round on 19.0. The log of the cluster was lost with the
  test's directory, so the cause is **not found**. It is the case the review of the whole part named (items 3 and 6: a leader's own lease lost on a loaded machine, a primary stopped by its watchdog while
  `kingresd` was late; the lease of these clusters is 12 s with a margin of 3 s); the output of a failing switchover test now has the lease, fence and stop lines of every server, so a next occurrence can be read.

### 2026-11-24 (second entry): P2, the live read of the primary, the switch point, the audit of the missing row

- **P2:** `abandonMapGroupBootstrap` takes an optional `lostServerId` (contract 0.28.0, additive, `kingres_admin`, audited with the server in its details). The server must not be heard by the server that answers (409 otherwise); the old leader then ends the hold of its primary without the word of that server when the hold was made for it (or has no record), it does not hear it either, every other server answers and none runs a bootstrap that names the old leader or was promoted. What the lost server promoted before it was lost is accepted with the declaration. Afterwards a new hard request may choose another standby (M1's refusal stands until the hold ends). Six mutants killed. **Console: API only** (`endpoints.abandonMapGroupBootstrap` takes the argument; the progress panel has no field for it). **Limit:** in a cluster with one standby of the map group, a dead candidate leaves no system database copy that sessions can be checked against, so every REST call is 503; the cluster test makes the old leader's resume call directly and shows that, with one standby, the declaration does not release the old leader (no other standby shows that nothing was promoted). The positive cluster test (a second standby of the map group) is not done; the unit tests show it.
- **M2:** the primary of the map group is asked (`pg_stat_replication`, state streaming, sync or quorum, and `pg_current_wal_flush_lsn()`, within three seconds) before the bootstrap starts and again right before the fence; it replaces the recorded check, and a primary that does not answer leaves the recorded facts. Four mutants killed.
- **Switch point:** the write probe emits a transactional WAL message on each instance (committed and acknowledged when the statement answers) and keeps the highest position; the test reads the last timeline history file of the new primary and asserts that its switch point is not before that position. The check of the old primary's WAL end with `pg_waldump` or `pg_rewind --dry-run` is not done.
- **M3:** the missing group 0 row is audited too (one mutant killed).

### 2026-11-24: review of the branch (P1, M1 to M3, P3 to P6) and the console

- **P1:** a group with the hand-over marker on disk counts as enforced everywhere in the lease manager (`enforced`), so an unstopped old primary is stopped by its lapsed lease although the plan lists no standby, and the idle refresh never keeps its watchdog file alive (also after a restart of kingresd). **M1:** the record of the candidate is not overwritten by another server and a failed fence removes the record it wrote. **M2:** the loss is judged again right before the fence. **M3:** a missing group 0 row pauses the bootstrap. **P3:** a hand-over that meets another bootstrap gives up. **P4:** a repeated request with another target is refused. **P5:** the post-stop alert is bounded. **P6:** a hard request with a proof is refused at the door. Each has a unit test that fails without it; the mutants were killed (P1: two tests; M1: three; M2: two; M3, P3, P4, P6: one each).
- **Console:** the refusal text for the map group, the hint that names the server that takes the repeated request, the generated schema for 0.28.0; vitest 1963 tests pass, `api:check` holds.
- **Write probe** in the hard failover tests (txid_current on the map group instance of the old and of the new primary every 20 ms): the old instance's last write is 2 to 3 s before the new one's first in the fence case and about 10 s before in the no-stop case (the watchdog); all five tests pass on 18.7 and 19.0.
- **The flake** (`TestSwitchoverSurvivesACleanStopThatOutlastsTheLease`, 18.7, 20 runs each): base 714b48be55: 14 passed, 6 failed (5 timeouts of 8 minutes, four in the step `Lift the freeze`); this tip: 19 passed, 1 failed (the same step: the old primary's server answers "waits for its group lease" and the step waits for it). It is not caused by the hard failover work; the cause (the unfreeze call to a former primary that is held back waits for minutes) is not fixed.
- **Not done:** P2 (an operator's way to declare a dead candidate lost), the LSN stronger tests (2: the switch point against the highest acknowledged LSN), the direct `pg_stat_replication` read right before the fence (M2 re-reads the leader's facts and positions instead), reuse of an earlier attempt's candidate by `stopOldMap` (a different candidate is refused with a message).

### Still open

- **Not done:** the escrow of the key of the certificate authority (an alert says what the operator does); the loops were run once per group (not 30 times: one round takes about 95 to 113 minutes on this machine);
  the independent review of the whole part after this increment (the design asks for one); the console has no text for the new pause (it shows the message of the status).
- The unexplained failure above.
- A hard failover of the map group when the old leader's kingresd is gone before the call of the fence is the failover of a dead leader (the bootstrap alone), as before.

## 2026-11-24 (branch p14c): the four low findings of the second review of 14c

L-A (the walk follows composite, array, range and multirange types), L-B (the marker stays on the final name until the rename is finished and the undoing only touches a database that carries it),
L-C (every relation of a distributed schema is listed, locked and checked; one that the archive does not name is refused) and L-D (`ledger_verify()` runs after the registered ledger tables are locked and
checked). The test hook `lastPrivate` is gone (`ImportHook` gets the private name). See the design note, "Changes made after the second review of 14c".

---

## 2026-11-24 (branch p14c): fixes after the security review of 14c

Answered C1, M1 to M3, L1 to L6 and the free-space check (design note, "Changes made after the independent security review of 14c"). New files: `ops/dbimport_guard.go` (the withdrawal of every role, the importer rule, the lock and
the catalog walk), `ops/dbimport_guard_pg_test.go` and `cmd/kingresd/dbimport_guard_e2e_test.go`. The importer may have `CREATEROLE` (a role of `kingres_admin` made through the API has it, and import works for it as it is); an importer with a member that can log in (a member with neither SET nor INHERIT, as the creator is, does not count) is refused, and `SUPERUSER`, `REPLICATION`, `BYPASSRLS` and the `pg_*` file, program and log roles stay refused. A test makes a login role as such an importer in the middle of an import.

## 2026-11-24 (branch p14c): phase 14c, database import

### What was done

- **Contract 0.30.0**: `importDatabase` (`POST /database-imports`), the closed `ImportDatabaseRequest`, the task kind `database.import`, the codes
  `archive_statement_refused` and `ledger_restart_not_acknowledged`, the checker rules, the README (174 operations, 139 paths), the generated types. It is its own commit (C0), and the console was built against it by a second writer.
- **The shape grammar in execution mode** (`sqlshape.Each`): the exact text that the grammar consumed completely, lexed again alone (a text that reads differently is refused); a refusal ends the walk.
  **`archive.OpenEntry`** reads an entry again and checks its size, CRC-32 and SHA-256 at the end of the stream; `archive.ReadManifest`; `archive.Assemble` and `archive/archivetest` for archives that lie.
- **The task `database.import`** (`ops/dbimport*.go`): the archive is read again from its bytes (file SHA-256 against `expectedSha256`, the validator, the grammar, the plan against the catalog, versions, the
  closed list of extensions, the ledger acknowledgement, `maxImportBytes`); a private database `kingres_import_<task id>` made strictly by the service role on every server, with a marker; the extensions; the
  function check against `pg_proc`; the statements of the archive **as the signed-in role**, one per round trip through the extended protocol with `search_path = pg_catalog`; the distribution from the catalog
  while the tables are empty (colocation groups, reference tables, columnar options, distributed schemas); the views; the data (one transaction per table, the rows against the manifest, reference tables through
  the temporary table of 14a); the constraints (a violated foreign key is counted and named); the sequences on every node; the ledger (new chain, `ledger_verify`, the policy set again) and the time-partition
  policies **disabled**; the verification; the rename on every server, the owner, the registry. The hook of a failed or cancelled task drops the private database (only one that it made).
- **The role is kept out while the service role works** (`CONNECT` is withdrawn and its sessions are ended, on every server, before every service phase), the service role touches a table of the role only after
  the catalog shows that it holds no code (`tableTrusted`, from the deparsed expressions: PostgreSQL records no dependency on a builtin function), and `extguard` (or the shard check of 14b) precedes every read of `kingres.*`.
- **V8 passed**: the rename of a database works on one and on three servers with the background workers connected. The fallback of the design was not needed.
- **Fixes of 14b that the import found**: the locale provider of a libc database was written as `builtin`; the colocation group of a distributed schema has no column type (the export failed with a 500).
- **Console** (a second writer, merged): the *Import database* dialog, the report page with the statements the plan refuses, the acknowledgement of the ledger restart, progress, the result and the failure text, the
  mock with the fixtures of the design, 2050 unit tests and browser and axe tests at three widths.

### How it was verified

PostgreSQL 18.7 and 19.0 in private installations (`~/kw-p14c/pg18` and `pg19`, the extensions built from this commit, each major built clean, deleted at the end), lane `l13`, `control-plane/scripts/run-integration.ps1`.

| Test | Layer | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|---|
| `sqlshape` (`Each`, hidden statements, the maximum, a fuzz target of 3.5 million runs), `archive` (`OpenEntry`, `ReadManifest`, `Assemble`), `ops` unit tests of the import (revalidation from bytes, plan, verdict about functions, names, steps, cancel guard, cached report, expressions), `transfer`, `problem`, `httpapi`, `config` | Go unit | pass | pass (same files) |
| `TestLexersAgreeWithTheServer` with 19 more scripts of the import statement kinds | PG test | pass | pass |
| `TestImportFunctionCheckOnARealCatalog`, `TestTableTrustedOnARealCatalog`, `TestExecOneIsOneStatement`, `TestUnqualifiedNamesDoNotLandAnywhere`, `TestForeignKeyViolationsAreCounted` | PG test, private install | pass | pass |
| `TestDatabaseImportRoundTrip`: every table kind, compared row for row, distribution, colocation sets, ledger (new chain, entries, `ledger_verify`, policy `required` back), policies disabled, sequences, owners, comments, views | cluster (one server) | pass | pass |
| `TestDatabaseImportRefusesHostileArchives`: 45 archives (comments, `\r`, dollar quotes, `SET ROLE`, `SET SESSION AUTHORIZATION`, `ALTER DATABASE`, a grant to PUBLIC on `pg_catalog`, extensions outside the list, an oversized statement, a zip bomb, manifests that lie, dangerous functions): refused, no database, role or setting left | cluster (one server) | pass | pass |
| `TestDatabaseImportHiddenTextIsNotRun`, `...RefusesADangerousRole`, `...WithdrawsTheAccessOfTheRole`, `...CleansUp` (failure, `keepOnFailure`, cancel), `...NeverUsesADatabaseItDidNotMake` | cluster (one server) | pass | pass |
| `TestDatabaseImportOfAnArchiveFromAnotherMajor` (18 into 19: imported and equal to its source; 19 into 18: `archive_incompatible`, nothing made) | cluster (one server) | pass | pass |
| `TestDatabaseImportOverThreeServers` (through followers, buckets in several groups, colocation, a distributed schema, ledger verification, the private database removed from every server after a failure) | cluster (three servers) | pass | pass |
| 14b regression: `TestDatabaseExport*`, `TestSpikeS3`, `TestDatabaseExportOverThreeServers` | cluster | pass | pass |
| Console: typecheck, lint, `api:check`, 114 files and 2050 vitest tests, `import-0-30` browser and axe tests at 1366, 683 and 320 px in both themes | web | pass | not applicable |
| Contract: `check-contract.mjs`, `httpapi` and `contract` tests | contract | pass | not applicable |

**Mutants** (one-line edits, each killed by the named test): the text of a statement is not lexed again alone (`TestSameTokensGuard`); the walk goes on after a refusal; a carriage return does not end a comment; the
grammar allows `SET`; the dangerous functions are not known; an extension outside the list is installed (`TestSetupRefusesAnExtensionOutsideTheList`); the checksum of an entry is not compared; `expectedSha256` is not compared;
the refusals of the grammar are not applied at import (both layers); a statement runs through the simple protocol (`TestExecOneIsOneStatement`); the session has the default search path; a table with a trigger of the
role is touched; the functions of the expressions of a table are not checked; the statements run as the service role (`TestDatabaseImportRoundTrip`); a private database that exists is a success; the final rename does
not check the name; `dblink` is in the closed list; the history rows of a ledger table are loaded; the ledger policy is not set again; time-partition policies are enabled; the failure hook drops nothing, or another
name; the access of the role is not withdrawn; the function check is skipped; a dangerous role may import; the rows are not compared with the manifest; the ledger restart is not acknowledged (`TestDatabaseImportRoundTrip`: the first import without the acknowledgement must fail). **All 27 are killed** (the runner and the list of the mutants are in the scratch area of the writer, not in the tree).

### Decisions and deviations

- A request does not read the archive (it cannot, for 100 GiB inside a request); a cached report only refuses early. Every hostile archive is refused by the first step of the task, with nothing made.
- No commit intent: a failure drops the whole database, so a table is one transaction with its count and nothing more. Statements are not grouped in a transaction per table.
- The service role works only while the role cannot connect (a design addition, "What the service role does after the role, and how the role is kept out of it" in the design note).
- The task drops a database only if the service role owns it and it carries its marker (a role with `CREATEDB` can make a database with a guessed private name).
- The free space of the data nodes against `maxImportBytes` is not checked; the leader change at every step is not tested with a real failover; no archive of 2 GiB or more was imported.

### Still open

- The integrator renumbers the contract (0.30.0 here) and the migration is none; the console and the server agree on the detail lines of the finished task (`ledger: ...`, `policy disabled: ...`, `NOT VALID: ...`).

### Core patches

None.

---

## 2026-11-23 (branch p14b, second entry): the fixes for the security review of phase 14b

### What was done

- A1: `extguard` (commit 13ae51e353 of `sec-ext`, taken alone) gates every read and call of the schema `kingres` in the facts of the export; an untrusted schema is a refusal (422 `schema_unrecognised`, or the task code). B7: a schema `kingres` owned by a role that is not a superuser, one that holds objects when no extension is installed, and a schema `kingres_transfer` that the service role does not own are refused. The facts are read under `SET LOCAL search_path = pg_catalog`, `pg_dump` has `PGOPTIONS=-c search_path=pg_catalog`, a password file of mode 0600 in the staging directory, a version check and a bounded tail of its error output in the server log (A2, B8); the role's connection sets `search_path = pg_catalog` (B9).
- B1, B2: line comments end at `\r`; dollar-quote tags may start with a high byte (`sqlshape` and `dumpnorm`). A corpus of harmless scripts is checked against a real server of PostgreSQL 18 and 19 (`TestLexersAgreeWithTheServer`) and is the seed of the fuzz targets.
- B3, B4, B5, B6, B10, P1, P2: a failed read is never a report; a versioned cache (an import validates again); ratio and expansion limits; sequence privileges; names as pairs (a dot is refused, the console keys by the pair); the report says that rows are claimed; the check for an export and the start of a table task are one step.

### How it was verified

| Test | Layer | PG18 | PG19 |
|---|---|---|---|
| `TestLexersAgreeWithTheServer` (22 scripts, the server counts the statements) | PG test | pass | pass |
| `TestLexerAgreesWithTheCorpus`, `TestCarriageReturnAndHighByteTags`, `TestSplitterAgreesWithTheCorpus` | Go unit | pass | pass |
| `TestFactsIgnoreTheSearchPathOfTheDatabase` (a shadowing schema first in the path of the database) | PG test | pass | pass |
| `TestExportRefusesAFakeKingresSchema` (a fake schema with a marker function, a full one and an empty one), `TestExportRefusesAFakeTransferSchema`, `TestExportIgnoresASearchPathOfTheDatabase`, `TestExportRefusesASequenceTheRoleCannotRead` | cluster | pass | pass |
| `TestAFailedReadIsNotAReport`, `TestArchiveLimitsFollowTheFile`, `TestCachedReportIsByTheChecksum` (version), `TestPasswordFileOfPgDump`, `TestRatioCap`, `TestDottedNamesAreRefused`, the keys of the console (`archive-keys-0-40`) | Go and web unit | pass | not applicable |
| the sweep of the first entry (export, spike, three servers, facts and dump) | cluster and PG | pass | pass |

Mutants of the review, all killed: the lexer of `sqlshape` or the splitter of `dumpnorm` ends a comment only at a line feed (`TestLexerAgreesWithTheCorpus`, `TestSplitterAgreesWithTheCorpus`); a tag may not start with a high byte (same tests); a failed read is made a report (`TestAFailedReadIsNotAReport`); a report of another validator version is used (`TestCachedReportIsByTheChecksum`); no ratio cap (`TestRatioCap`); a dot accepted (`TestDottedNamesAreRefused`); the guard and the check of the fixed schemas removed, and the owner of the schema `kingres` not checked (`TestExportRefusesAFakeKingresSchema`; the owner mutant needed the empty schema that was added); the sequence privilege not checked (`TestExportRefusesASequenceTheRoleCannotRead`); the search path of the facts not pinned (`TestFactsIgnoreTheSearchPathOfTheDatabase`; the end-to-end test could not show it, because the service connection already pins the path in its startup parameters); the schema `kingres_transfer` not checked (`TestExportRefusesAFakeTransferSchema`).

### Decisions and deviations

- The commit of `sec-ext` was cherry-picked alone; the contract text of its alert kind moved into 0.29.0.
- The tail of the error output of `pg_dump` goes to the server log (the reviewer asked for it); it can name objects.

### Still open

- 14c validates the archive again at import time and never trusts `archive_state` (design note above).

### Core patches

None.

---

## 2026-11-23 (branch p14b): phase 14b, database export and archive validation

### What was done

- **Contract 0.29.0** (the number it has in g2; 0.40.0 on the branch): `exportDatabase` and `validateDatabaseArchive`, `ExportDatabaseRequest` (closed), `ArchiveReport`, the upload purpose `databaseArchive`, the task kinds `database.export` and `archive.validate` (subject type `upload` is new), five problem codes, the audit actions, `DataExport.warnings`. The checker has the rules of 0.29.0, the README has the section "Database archives" and the changelog; `web/src/api/schema.d.ts` and the operation table are generated (`npm run api:check` and `go generate` agree). Migration `0050_archive_validation.sql` (a placeholder number) adds the purpose of an upload and the state of an archive validation.
- **Spike S3** was **not done before**; it was done first, on PostgreSQL 18.7 and 19.0, and its result is written into the design ("Results of the spikes and changes made by the code (phase 14b)"). The dump of the two majors is the same except for the version comments and the `\restrict` token. The golden files are `internal/dumpnorm/testdata/pg18` and `pg19`.
- **Package `dumpnorm`**: the normaliser of the dump (removals and a closed set of rewrites, views moved, the user's partitioning of a distributed table synthesised from the catalog).
- **Package `archive`** (writer, validator, manifest and catalog decoders, version rules, report; by a second writer) and **package `sqlshape`** (the lexer and the shape grammar in validate-only mode; by a third); both were merged into `p14b`.
- **The task `database.export`** (`ops/dbexport*.go`): the request checks (unsupported objects, large objects, the privileges of the role on every table, code of other roles), the structure in one `REPEATABLE READ READ ONLY` transaction of the service role with `pg_dump` in its snapshot, the normalisation and the run of the shape grammar over the result, the data written to the streaming zip writer as the role (one transaction when the archive can be one snapshot), the check of the file by the validator and of the fingerprint, the seal. **The task `archive.validate`** (`ops/archivevalidate.go`): the report stored with the upload, cached by the SHA-256 of the file.
- **Console** (`kingres/web`; a fourth writer): the export dialog, the validation dialog and the report page, the Transfers panel for archives, the mock with the fixtures of the design, 2009 unit tests, and browser and axe tests at three widths.

### How it was verified

The test table is below. PostgreSQL 18.7 and 19.0 in private installations (`~/kw-p14b/pg18` and `pg19`, built from this commit, deleted at the end), lane `l13`, with `control-plane/scripts/run-integration.ps1`.

| Test | Layer | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|---|
| `dumpnorm`: goldens of both majors (`TestGoldenNormalisation`, `TestMajorsAgree`, `TestRefusals`, `TestOtherStatementsAreKept`, `TestSplitter`) | Go unit | pass | pass (same files) |
| `archive` (19 tests, the hostile corpus of 66 archives, 3 fuzz targets) and `sqlshape` (tables of 47 accepted and 110 refused statements, 2 fuzz targets) | Go unit | pass | pass |
| `ops`: facts, catalog, normaliser facts, labels, entries, fingerprint, exporter output accepted by the validator, warnings, refusals, dump arguments, cache | Go unit | pass | pass |
| `TestReadFactsAndDumpOfARealDatabase`: the catalog read, `pg_dump` of the build, the normaliser and the grammar, on a real instance | PG test, private install | pass | pass |
| `TestSpikeS3DumpOfEveryTableKind` | cluster (one server) | pass | pass |
| `TestDatabaseExportOfEveryTableKindAndItsValidation` (local, reference, hash, user-partitioned, columnar, updatable and append-only ledger, time-partitioned, inheritance, partitioned, views; download; validation of the upload; a damaged archive) | cluster (one server) | pass | pass |
| `TestDatabaseExportRefusals` (unknown database, bad options, a function, large objects and their acknowledgement, a role that cannot read: 20 of 25 named, excluded table, data mode none) | cluster (one server) | pass | pass |
| `TestDatabaseExportBlocksTableTasksAndNoticesChanges` (a table task refused during an export, a cancel leaves no file, a column added fails with `schema_changed`) | cluster (one server) | pass | pass |
| `TestDatabaseExportTooLarge` (`export_too_large`, no file, then the same database under a larger limit) | cluster (one server) | pass | pass |
| `TestDatabaseExportOverThreeServers` (per-group labels, a follower makes it, three servers serve the download, an upload through a follower validated through another) | cluster (three servers) | pass | pass |
| Console: typecheck, lint, 2009 vitest tests (111 files), `archive-0-29` browser and axe tests at 1366, 683 and 320 px in both themes | web | pass | not applicable |
| Contract: `check-contract.mjs`, `api:check`, `httpapi` and `contract` tests | contract | pass | not applicable |

Mutants (one-line edits, each killed by the named test; the script is not part of the tree): see the report of the unit of work; the list is in "Mutants" below.

### Mutants

| Mutant | Killed by |
|---|---|
| the normaliser keeps ONLY on the constraint or ON ONLY on the index of a distributed table | `TestGoldenNormalisation` |
| the capture triggers of ledger tables are not removed; a trigger of that name on any table is removed | `TestGoldenNormalisation`; `TestOtherStatementsAreKept` |
| PARTITION BY HASH on another column is accepted; every SET is accepted; another table access method is accepted | `TestRefusals` (three cases) |
| USING columnar is not added; views stay in pre-data; a bucket partition is not removed | `TestGoldenNormalisation` |
| a GRANT is dropped instead of kept for the grammar to refuse | `TestOtherStatementsAreKept` |
| a partition leaf is exported (rows twice) | `TestDataEntriesAndLabels` |
| the history entry names the history table | `TestExporterOutputIsAcceptedByTheValidator` |
| the fingerprint ignores the columns | `TestFingerprintChangesWithTheStructure` |
| large objects are not counted; objects that the archive does not carry are not refused | `TestCheckExportableRefusesWhatTheArchiveDoesNotCarry` |
| the label is always single-snapshot; the archive is always one snapshot | `TestDataEntriesAndLabels` |
| the cache of a validation ignores the checksum | `TestCachedReportIsByTheChecksum` |
| the LIKE escape is a no-op; a columnar table is estimated like a heap table | `TestSplitPartKeyAndEstimates` |
| the warning for a table read per group is not written | `TestWarningsSayWhatTheArchiveDoesNotCarry` |
| pg_dump takes no snapshot; pg_dump keeps PGPASSFILE and PGSERVICE of the process | `TestDumpArgumentsAndEnvironment` |
| the privilege check of the role is removed | `TestDatabaseExportRefusals` (cluster) |
| the fingerprint is not compared at the end; a table task is not refused while an export runs | `TestDatabaseExportBlocksTableTasksAndNoticesChanges` (cluster) |
| the limit of the largest file is not applied | `TestDatabaseExportTooLarge` (cluster) |
| every table is labelled by its own statement although one transaction is used | `TestDatabaseExportOfEveryTableKindAndItsValidation` (cluster) |
| data mode none still writes the data; an excluded table keeps its data | `TestDatabaseExportRefusals` (cluster) |

All 31 mutants of this list were killed (11 of the normaliser, 13 of the unit tests of the export, 7 in the cluster). The writer of the archive package killed 15 (the writer omits a descriptor; the validator ignores the local name, the local extra field or the descriptor flag; the overlap, hidden-byte, compressed-extent, CRC-32, SHA-256 and entry-count checks removed; the declared-size reader is +1000; the PostgreSQL major compare reversed; the extension version compare is `>`; a symbolic link accepted; a comment in the end record accepted); the overlap-check mutant survived the hostile archives and is killed by `TestExtentsRefuseAMovedOffset`, which calls the check directly. The writer of the shape grammar killed 16 of 17 (the survivor, `CREATE TABLE ... AS` accepted, is equivalent: a second guard refuses it).

### Needs-verification items

- S3 / V1 / V2: done (above). V8 (rename of a database) and the rest of 14c: not checked, as the phase says. V9: a read reaches virtual generated columns only, because policies are refused.

### Decisions and deviations

See "Changes made by the code" in the design: the strictness of the archive shape, the progress in the task detail, a restart of the whole data step, the decision of the single snapshot by the catalog, the refusal of row-level security, the history entry naming its table, the exclusion of table tasks.

### Still open

- Phase 14c: the import (the private database, the shape grammar's import steps, the distribution before the data, the ledger and the policies), `expectedSha256`, the console import dialogs. Not covered by tests: a leader change at every step of the export, a distributed schema, a columnar distributed table, an archive above 2 GiB.

### Core patches

None.

---

## 2026-11-22 (staging branch g15): cp-1011, p13-fix and web-15b merged, contract revisions renumbered

The staging branch g15 (from kingres-18) merges cp-1011, p13-fix and web-15b. The contract revisions are renumbered in merge order so that they follow kingres-18's 0.14.0: the control plane of phases 10 and 11 (written as 0.16.0 in cp-1011) is 0.15.0, and the closing of phase 13 (written as 0.17.0 in p13-fix) is 0.16.0. Entries of this log and the review files that were written on those branches still say 0.16.0 and 0.17.0; read them as 0.15.0 and 0.16.0. The failover branch, which carries an older 0.15.0, is renumbered when it merges. Generated files were regenerated; no migration numbers collide (cp-1011 has 0040 and 0041, p13-fix has none).

---

## 2026-11-22 (second entry): the fixes for the review of the fixes of phase 13

### What was done

- The review ([reviews/2026-11-22-config-phase13-fixes.md](reviews/2026-11-22-config-phase13-fixes.md)) found 5 medium and 4 low findings (P13S-01 to P13S-09, no critical or high). All are fixed or answered on `p13-fix`; the report has a "Fixes" section.
- P13S-01: every successful start keeps the override file as the last good one; the manager retries a failed start with it (the empty file when none, never the previous one), for every start (the restart task, a reboot, a crash, an operator), with a mark the task reads.
- P13S-02, 03, 06, 09: the sample-rate log parameters and `log_file_mode` are managed; `archive_mode`, `client_encoding`, `restart_after_crash`, `recovery_min_apply_delay`, `max_slot_wal_keep_size` are refused. P13S-04: floor of 8 for `max_worker_processes`.
- P13S-05: the hash of a server covers every instance, and a copied standby gets the override file of its own server. P13S-07: `expectedRevision` is checked again in the record step, the start of a configuration task is one step under a mutex, the dialog waits for the revision.

### How it was verified

- Mutants: 5 for the rollback and the identity (killed by the new tests), 7 for the lists, the floor, `supersedes` and the standby file, 1 for the dialog; all killed.
- PostgreSQL 18 and 19.0 (private installs, lane l5): `pgconfig`, `instance`, `httpapi`, `ops`, `standby` packages (318 passing tests each) and the 23 `TestConfig*` cluster tests.
- Console: typecheck, lint, 1464 unit tests, `api:check`; `check-contract.mjs` holds. The browser tests were not run again (only a text and a disabled state of the dialog changed since the last run).

### Decisions and deviations

- The exclusion of `config.apply` and `config.restart` is a mutex of the leader, not a rule of the engine (see the report).

### Still open

See "What is left" of the report: no state on the server page for a start that went back to the good file at a reboot; no floors for `max_parallel_workers` and `max_locks_per_transaction`.

## 2026-11-22 (third entry): phase 11 leftovers in kingres_shard: the schemas in the rebalance plan and the drain, and the schema checks of check_cluster

Covers branch p11-left (from shard-p12), design [shard-phase11.md](design/shard-phase11.md) sections 9.9 and 9.10.

### What was done

- **The rebalance plan.**  `kingres.plan_rebalance()` and `group_load()` now plan a distributed schema as an item `(colocation_id, 0)` like a bucket (before, the plan counted the schema
  in the load of its group and never moved it).  A schema that a ledger table pins is not an item and stays as fixed load; the owners are asked for the pins
  (`internal_pinned_schemas()`, node-local) when the caller is a superuser.  The sizes under `by_disk_size` were already read from the owner (review P10R-09).
- **The drain.**  A drain is a rebalance with `drain_only`, so its plan holds the schemas of the group with `reason = 'drain'`.  New: `kingres.schemas_on_group(group)` (superuser), the schemas of
  a group with sizes and blockers (a ledger pin, an unlogged table, a table with neither a key nor a full replica identity), which is what the preview of a drain needs.  `set_node_property(node, false)`
  already kept new schemas off the group and `remove_node` already refuses a group that owns a bucket; both are now tested for schemas.
- **`check_cluster`.**  New `internal_check_schemas()`: for every active primary, `schema_missing`, `schema_table_missing`, `schema_structure_differs` (the table fingerprint), `stray_schema_rows` (rows in the
  empty copy of a table of another group's schema, outside a move that has the group as source or target) and `schema_catalog_differs` (a digest of the schemas, their groups, states, placement epochs and
  numbers of tables, by names).  The node does the work (`internal_check_schemas_local`) from what the map group primary expects.
- Tests `t/276` (the plan, the pinned schema, the drain, placement, removal) and `t/277` (every check, a move, a clean cluster); `t/236` section C now says what it always meant (the plan of phase 4 does not move a schema).
  Each change has a mutant that a test kills.
- **SQL.**  The changes are in `kingres_shard--0.8--0.9.sql` (0.8 is released and its scripts are as they were): `CREATE OR REPLACE` of `internal_plan_inputs()` and `check_cluster()` and a block of six new functions; `kingres_shard--0.9.sql` is regenerated with `mk-install-script.sh`.  The branch contains `p12-storage` (kingres_shard 0.9, columnar) merged in; the tests of this work are `t/276` and `t/277` (`t/270` to `t/275` are the columnar and count-distinct tests of p12-storage, which `meson.build` now lists too).

### How it was verified

Full run on both majors (kingres_shard TAP: 235 files, 59620 tests on 19 and 59060 on 18, all pass; regress, kingres_ledger and kingres_columnar pass), and in-tree meson (kingres_shard 234 ok, 1 skipped; ledger 39 and columnar 69 ok).  One in-tree failure, `t/188_online_alter` (a warning that the verification of a table operation was not done: 402 rows against 400 after the switch, under 4-process load), passed 3 of 3 reruns alone and in both TAP runs; it is watched as a load-dependent flake.

### Decisions and deviations

- A non-superuser's plan does not know the pins (only a superuser can ask the nodes); the move of a pinned schema is refused when tried.
- `drain_status()` is unchanged (it counts a schema's bucket among the buckets of its group); the schemas of a group come from `schemas_on_group()`.
- The message of `remove_node` still says "buckets", not "schemas" (the function is in `node_admin.c`, which the 0.9 work changes).

### Still open

- The step 6 of the drain in the control plane (reading the schemas again) and `previewServerDrain` belong to cp-1011 and can now call `schemas_on_group()`.

## 2026-11-22: failover round 2 (restore, planned switchover, the map group with a live leader), its independent review and the fixes

Covers the branch fo-r2wip, rebased onto kingres-18 at e279c14091 (contract 0.15.0).

### What was done

- **Round 2 of failover:** the restore of the preferred primaries, the plan and phases of a planned switchover, and the map group with a live old leader (a planned switchover
  and a restore that include group 0): the bootstrap side in `mapgroup`, the leader's side in `failover`, the signed start of an operator's request (ADR 0026), the call that
  gives up a stuck bootstrap (`abandonMapGroupBootstrap`, contract 0.15.0), `HoldBack` (the lease is renewed until a clean stop is over), and the 15 s bound on calls to the old
  server.
- **Independent review**
  ([reviews/2026-11-21-failover-round-2.md](reviews/2026-11-21-failover-round-2.md)): no critical finding, one high (F5-01), five medium, six low. All twelve are fixed, one commit
  per finding; the table in the review maps them to commits.
- **Three defects found by the cluster tests of the fixes** and fixed: the step of the map group ended with the task lease about ten seconds after the clean stop (no rollback ran);
  a rollback that could not be recorded made the engine run the step again and start a second switchover; and a nil dereference in the lease manager (`writeWatchdogFile`) crashed
  kingresd in the rollback.
- **Web:** the generated files and the panel follow contract 0.15.0: audit words, the endpoint, the mock handler, and a button on the failover progress view that gives up the move of
  the manager (`kingres_admin`).
- **Test hardening:** the request of the map group failover tests waits out, within two minutes, the 422 of a leader whose view does not yet show the killed server dead.
- **Second independent review**
  ([reviews/2026-11-22-failover-round-2-fixes.md](reviews/2026-11-22-failover-round-2-fixes.md)): all F5 fixes hold (22 mutants killed); one high (F6-01: the old leader of a planned
  switchover of the map group can stay held back with no primary, and nothing repairs it), two medium (F6-02, F6-03), three low (F6-04 to F6-06). All are fixed (the table in the
  review maps them to commits): the old leader ends a hold that nothing owns by itself, the candidate and the operator ask it to, the abandon works from the control data of a
  standby it cannot ask and waits for a promotion that is running, and a bootstrap that was given up cannot promote (the lane found the last gap).
- **Third independent review** ([reviews/2026-11-22-failover-f6-fixes.md](reviews/2026-11-22-failover-f6-fixes.md)): no path to two writable primaries; one high (F7-01: the old
  leader resumed on the evidence of any standby in recovery, so after a completed switchover whose new primary died it could revive stale data), one medium (F7-02: a failed release of the
  lease did not fail the abandon), three low. All fixed: the old leader resumes only on the word of the server the group was handed to (recorded at the clean stop), a failed release fails
  the abandon, an unresolved hold is warned about once a minute and shown in the status.
- **The report of an old primary** that needed crash recovery carries the divergence in bytes (zero when its WAL ends before the point where the histories parted), measured from its
  shutdown checkpoint; the cluster test of the old primary accepts a missing number only with the documented message.

### How it was verified

- Unit tests of failover, journal, mapgroup, kingresd, lease, internalapi, httpapi and contract pass; a mutant of every fix (F5-01 to F5-12 except the two documents, and the three
  later fixes) fails the new test of the fix and is killed. `check-contract.mjs` holds for 0.15.0 (141 operations); `go generate ./internal/contract` changes nothing.
- Cluster tests in lane l3 on PostgreSQL 18 and 19 with kingres_shard 0.7 (extensions of e279c14091): the switchover, abandon and restore set and the failover, old-primary and
  map-group set pass on both majors, with the five new tests of the review (a candidate that never promotes, an operator abandon, an abandon while the promoting call is in flight,
  a standby that is behind, an unreadable `pg_controldata`). `TestMapGroupBootstrapSurvivesKingresdEnding` failed once on PostgreSQL 19 with the 422 described above and then passed
  three of three reruns; it was hardened as described.
- F6 and F7: 24 mutants of the F6 fixes and 17 of the F7 fixes (lists in the reviews) are killed. The last full lane l3 run (tip 6170828196, PostgreSQL 18 and 19.0 reinstalled from the
  reference install, extensions of e279c14091) passed the three new cluster tests on both majors, the switchover and abandon set on PostgreSQL 18, and the failover, old-primary and
  map-group set on both majors. The switchover and abandon set on PostgreSQL 19.0 failed once, in `TestSwitchoverOfTheMapGroup` (timed out after 10 minutes waiting for the task on the
  new leader, which answered 503); the other tests of that step passed. Its log was too short to show a cause, and the test passed in 4 of 4 reruns alone (3 on PostgreSQL 19.0, 1 on
  PostgreSQL 18). The cause is not known; the output of the test on failure was enlarged to show it next time.
- Web: typecheck, lint, `api:check`, `brand:check` and the full vitest suite pass. The browser and end-to-end suites were not run.
- Not verified: the review's item 5 (the candidate killed between a refusal and the abandon) has no cluster test of its own; unit tests cover it, and the second case of F6-01
  (the old leader restarted) is a cluster test now. The tests of `internal/pgconninfo` could not run on the Windows host (the test executable is blocked); they are not related to this work.

### Needs-verification items

None checked in this entry.

### Decisions and deviations

- ADR 0026 (the candidate of a map group failover receives the proof of the operator's session) is accepted as the way to carry an operator's authority to a member that is not the
  leader; the documents say so.
- After the clean stop of the map group the step runs on a context that is not cancelled (bounded by the verdict wait of six minutes), because the engine cancels its context when the
  task lease cannot be renewed.

### Still open

- The map group's hard failover of a live leader's server is still refused (422); only the planned switchover and the restore include group 0.
- A cluster test for the candidate killed between a refusal and the abandon (unit tests cover it).
- The old leader's resolution of a hold waits for a standby to be shown in recovery; with every other server gone it does not resume (the lease would protect it, but the proof is
  missing): an operator starts it by the abandon call once a standby answers.

## 2026-11-22: the search_path of the privileged SQL of kingres_shard (XSP-01 to XSP-03)

Covers the fix of the three findings of the static audit `extaudit/2026-11-22-extension-search-path-static` on branch xsp-fix (kingres_shard 0.8).

### What was done

- **XSP-01** (`claimed_relid_check`, `utility.c`) and **XSP-02** (`kingres_shard_assert_no_move`, `bucket_move.c`): both run SQL as the bootstrap superuser from the ProcessUtility hook, in the session's own `search_path`, and
  their `phase IN (...)` lists were parsed with an unqualified `=`.  A database owner (`ALTER DATABASE ... SET search_path`) could plant an `=(text, text)` operator in a schema of its own and have its function
  run as the bootstrap superuser.  Each block now opens a GUC nest level and forces `search_path = pg_catalog, pg_temp` (as `acl_query()` in `node_admin.c` does) until the block ends, and every list is
  `OPERATOR(pg_catalog.=) ANY (ARRAY[...]::pg_catalog.text[])`.
- **XSP-03** (hook statements that run as the calling role): `is_user_partitioned`, `user_partition_exists` and the DROP TABLE lookup of `map_partition_statement` in `utility.c` write every comparison as
  `OPERATOR(pg_catalog.=)`.  They do not force the path: the caller's own path is what resolves the names of the user's statement elsewhere, and the statements name no object but `kingres.*` and `pg_catalog.*`.
  The text of `schema_ctas` is left as it is: it replays the user's own query, and adds no operator.
- **The sweep of the rest of the source**: every SPI statement and every raise of privileges in `contrib/kingres_shard/src` was read again.  The other raises (`schema_ddl.c`, `node_admin.c`) already call only
  `kingres.internal_*` functions or use `OPERATOR(pg_catalog.x)`; the other statements with `IN`, `=` or `LIKE` run inside SQL-callable functions or workers that pin `pg_catalog` first.  Nothing else changed.
- **`t/252_extension_search_path.pl`**: a database owner without superuser rights plants `=(text, text)` and `=(name, text)` in its own schema (the functions only record the call, in a table the owner owns), sets
  `ALTER DATABASE ... SET search_path = evil, pg_catalog`, and runs ADD COLUMN, CREATE INDEX, CREATE TABLE IF NOT EXISTS ... PARTITION OF and DROP TABLE IF EXISTS on its distributed tables.  History rows of a finished
  table operation, move and split are in the catalog, because an operator is called only for a row that the statement reads.  The test asserts that no planted function ran and that the DDL reached both nodes.
- README, "Security notes": one item on the forced path and the qualified operators.

### How it was verified

- The new test passes on 18.7 and 19.0.  Four mutants, each built with -Werror and each failing test 11 (calls of the planted operator): the sources of `utility.c` and `bucket_move.c` as before the fix (29 calls), `utility.c`
  alone as before (13), `bucket_move.c` alone as before (16), only the qualification of the XSP-03 statements removed (3).
- The full `kingres_shard` suite (clean -Werror build, regress, every TAP file) on PostgreSQL 18.7 and 19.0: 219 TAP files, 48074 tests on 18 and 48770 on 19, all passed (one file skipped by design on each major, as always).  `kingres_ledger` built into both installations first.

### Decisions and deviations

- The XSP-03 statements are qualified but not run under a forced path, on purpose (see above).
- The block of the two privileged checks has no PG_TRY: an error inside it ends the transaction, which restores the GUC and the user id.

### Still open

- Nothing from the three findings.

### Core patches

None.

---

## 2026-11-22 (second entry): phase 12, the sharding part: the repartitioned join and MERGE on distributed tables

Covers branch shard-p12, rebased onto the staging branch m18 (kingres-18 with the 0.8 tree of shard-p1011 merged).  Design:
[analytics-phase12-queries.md](design/analytics-phase12-queries.md), sections 3 to 7; the sketch extension and the rollup tasks of that note are not part of this entry.

### What was done

- **The base.**  shard-p12 (J1 and J2 step 1, built on shard-p10) was rebased onto m18; one conflict, the settings list of `expected/catalog.out`.  No SQL object is added by any of this work
  (the design says the upgrade script stays a guard), so there is still one upgrade path and `kingres_shard--0.8.sql` is unchanged; `t/250` passes.  Two scratch scripts that had been
  committed at the repository root were removed.
- **The repartitioned join** (`src/repartition.c`, `src/fragment_scan.c`): J1 and J2 step 1 as before, then a partial aggregation above the repartitioned join (the groups aggregate their rows of
  the join, the origin combines; the partial marker carries `rp=<n>`), the behaviour under bucket moves (fault points `rp_after_ship` and `rp_before_fragment`; a stale map gives `KS001`, never a wrong
  answer; `t/262`), the equivalence generator `t/267` (seeded, four profiles against the single node), regress `repartition_plans`, the `EXPLAIN` lines of design 4.12 (what moved, or "Repartition: not used:
  <reason>"), and a broadcast side (a table that has no routable key is sent whole to every group; inner joins and the nullable side of a LEFT JOIN; bounded by `repartition_max_bytes`).
- **`MERGE` on distributed tables** (`src/merge.c`, `src/planner.c`, `src/fanout.c`): the settings `enable_distributed_merge` and `merge_max_source_bytes`; M1 (a colocated MERGE runs on every group that owns a
  target bucket), M3 (a routed source: the origin reads the source and sends each row to the owner of its key), M6 (a local target, a ledger table included, with a source fenced by `OFFSET 0`, which
  works with the real planner on both majors), M4 and M5 (a reference target with constants or reference tables, or a source that the origin reads once and sends to every node); fault points
  `merge_before_first_group`, `merge_between_groups`, `merge_before_commit`; tests `t/263` to `t/266` and regress `merge_plans`.  Every shape that is not accepted is refused with its own message, as before.
- The tests and mutants of each piece are in the commits; every mutant that survived is explained there (defence in depth, cost-only, or equivalent).

### How it was verified

- At the tip (clean -Werror builds, regress, every TAP file) on PostgreSQL 18.7 and 19rc2: 226 files, 57203 tests on 18 and 57234 on 19; the one failure was `t/043_reference_paths` (it expected the old
  refusal of `MERGE` into a reference table, which M4 now runs; the test was changed and passes).  In tree: see the message of the integration.
- The neighbours of the changed code (t/112 to t/160, t/260 to t/267) were also run by the authors of each piece on PG19.

### Decisions and deviations

- A preserved moved side of a LEFT JOIN, dual repartition, a closed whole-statement fragment above the repartitioned join, and extension objects in the text of a repartitioned join are **not built**: each is
  refused with its reason and the complete scan answers.  The reasons are in the README ("The repartitioned join").  A preserved moved side is believed provable (NULL keys go once to one group; ON conditions on
  the preserved table stay in the text; no pruning from the nullable side) but needs a partitioned role for a shipped relation in the group's analysis, which is shared with the closed queries of 2b.
- The marker of a MERGE fragment is `kingres:merge-fragment place=N rp=K`, in the style of the existing kinds, not `kingres:frag v=2 kind=merge` as the design says.
- M3 and M5 need the source to be one relation (a join in `USING` goes in a sub-select); M1 accepts a join of colocated tables.  A key restriction in `ON` does not prune the groups of M1 (always correct).
- A NULL route key is hashed like a NULL in the row router and in tuple routing, which is the design's slot-0 rule without a special case.

### The fixes for the review

The independent review of this branch ([2026-11-22-kingres-shard-phase12-joins-merge.md](reviews/2026-11-22-kingres-shard-phase12-joins-merge.md);
the repartitioned join held, MERGE had one silent wrong answer) is fixed on branch p12-fix, one commit for each finding, each with a test that fails
without it (the section "Fixes" of the review lists the commits, the tests and the mutants).  P12M-01: a prepared MERGE that declares a parameter it does not
use no longer shifts the arrays of a shipped source (the origin sends the parameters that the text uses, then the arrays; `t/268` section A, nine types in
three positions for M1, M3, M4, M5 and M6).  P12M-02: a MERGE through a view is refused explicitly, and the guards for the target (CHECK OPTION, row-level
security, view owner, view) run on the statement as written for every shape.  The fixing found a case that the review had not: a security_barrier view over a
reference table was accepted by M5 after M4 had refused it, and the text of M5 has no filter, so every node changed rows that the view hides; the guards
now run before M4 and M5 are tried.  P12M-03: a source row of a group that is not a planned target is KS001, not dropped (new fault point
`merge_before_collect`).  P12M-04: the merge fault points fire for M4 and M5 (`t/266` section E).  P12M-05: two stale README passages.  P12M-06: the refusal for
a stable default names the column.  P12J-01: `repartition_max_bytes` is capped at 512MB.  P12J-02: the group-side rule for a preserved moved side is pinned
by forged fragments (`t/261` section G).  Not changed, and open: M5 takes the writer lock of every relation of the statement on every node, so a local source
that exists on one node only is refused with "reference table ... does not exist on this node" (a refusal, not a wrong answer; the lock names should come from
the reference tables only).

### Still open

- The four items above that are not built.
- An independent review of the fixes of the review of this branch.
- M5 and a local source that does not exist on every node (see the fixes above).
- Integration with phase 10's scheduler and the control plane of cp-1011 (`MERGE` and the repartitioned join add no REST surface).

## 2026-11-22 (third entry): fixes for the second review of the phase 12 control plane (P12D-01 to P12D-09)

## 2026-11-22: the console of phase 12 follows the settled contract, the owners of tables, and the independent review

Continues the entry below on branch p12-web.

### What was done

- Merged p12-cp twice (`22a40eb52f`, `a8a770e771`; contract still 0.18.0): the `dryRun` preview and typed blockers of a storage change, the table in the alert subject, sentence step names, the enumerated shapes of `partitionEvery`, the bound of `asOf`, a put refused during a run, the cluster setting `analytics.allowNonSuperuserOwners` (a switch on the Settings page with its consequence and a confirmation to turn it on), `tableOwner` and `ownerTrusted` (an owner notice with a link to the setting), the blocker `owner_not_trusted`, and the refusal of a policy put and run for an owner that is not trusted. The mock follows each of them.
- Fixed the findings P12W1 to P12W10 of the independent review ([the review](reviews/2026-11-22-console-phase12.md), section Fixes): sizes never made up, no Save over a policy that was not read, Run now held and red, a re-enabled drop policy needs preview and name, values the form cannot show are kept, real multi-unit intervals, a mock that finds indexes and triggers only when the task runs, field errors by JSON pointer, the smaller points.

### How it was checked

- typecheck, lint, `api:check`, the full vitest suite and the browser suite: full vitest 95 files, 1727 tests pass; browser suite 4 files, 48 tests pass; typecheck, lint and api:check clean.
- New mutants, all caught: trusting owners without asking, a policy put accepted for an untrusted owner, the setting not making an owner trusted, the owner notice for a trusted owner, a run for an untrusted owner not refused, Save over an unread policy, Run now not held on a failed plan, a kept value rewritten, a drop policy turned on without preview, a size shown as 0, a 404 of another instance taken for no policy, field errors read by the bare name, a run that sends `asOf`, a plan with invented sizes, an unknown format counted as Row; the earlier survivors M6, M8 and M17 of the review are caught by tests of their own.

### Still open

- The end-to-end run against a real `kingresd`.
- Not pushed.

---

## 2026-11-22: the analytics console (phase 12, steps W1 and W2) on contract 0.18.0

Branch p12-web, from the contract commit `7d43a79a3f` of p12-cp. The table page of increment 9.7 (branch web-15b, one commit) is not on kingres-18, and the Storage column, the partitions panel and the policy need it, so that commit is taken first (cherry-picked, the same change). The control plane of phase 12 is built on p12-cp by another writer; nothing in `kingres/api` or in Go is changed here.

### What was done

- **Approximate aggregates** (inventory row Q-W1): `ApproximateAggregatesCard` on the database page and `ApproximateAggregatesDialog` (a switch, the mode of count(DISTINCT), the error rate in percent, the consequence in words, the task with its five steps). The three new problem codes of the extension have their own words.
- **Plan summary** (Q-W1, and the phase 2b item "EXPLAIN path summary in the workbench"): `lib/plan-summary.ts` reads the lines `Pushed:`, `Repartition:`, `Pushdown: not used:`, `Rewritten:` and `Remote groups:` of an EXPLAIN result and shows one line above the monospaced plan (path, why not, rewrite); no line it knows, no summary. The help text with the sketch functions appears when the database has the extension; the note says EXPLAIN ANALYZE and MERGE run the statement.
- **Storage** (St-W1): `StorageBadge` and the Storage column and filter of the tables list; the Storage row, the format, range, rows and ratio of each partition, a filter by format and a marker with progress on the row of a partition that is being converted, on the table page.
- **Policy and runs** (St-W1, St-W2): `PartitionPolicyPanel` (policy, health line, next and last run, runs list), `PartitionRunDialog` (a run with its actions, failures in words), `PartitionPolicyDialog` (the form, **Preview** through `planPartitionPolicy`, the field errors from the JSON pointers, the table name typed when a retention is new or shorter, and a preview of the form as it is then), `RunPolicyDialog` (the plan first, then the task), `RemovePolicyDialog`, and the link of the two new alert kinds to the table.
- **Convert storage** (St-W2): `SetStorageDialog` for a table or one partition (codec, level, stripe and chunk rows, sort order of a partition, what the console already knows that stops it, the reasons of `conversion_refused` listed, cancel until the switch).
- **Mock** (`src/mocks/analytics.ts`, switch `?analytics=`): all eleven operations as the contract says, including the tasks, the problem codes, the idempotency of configure and put, the dry run that changes nothing, the 404 of a table without a policy, the roles and `unsupported_operation` on an older server. `describeTable` adds `storage` and the new members of `partitioning`; `listTablePartitions` answers the format, range, rows and ratio.
- README of the web panel: a section for the analytics console and the switch `?analytics=`. The notes on what the contract does not say are in the report of the writer (11 items: the subject of the partition alerts, no preview of a storage change, no way to install kingres_columnar, the step names, the shapes of the intervals, and others).
- A separate commit formats three files that were unformatted before this work (`api/problem.ts`, `contract-0-4.test.ts`, `kind-labels.test.ts`); `npm run lint` fails without it.

### How it was checked

- `npm run typecheck`, `npm run lint` (eslint and prettier) and `npm run api:check` (the generated files are up to date with the contract at `7d43a79a3f`): clean.
- `npx vitest run` (two workers): 95 files, 1685 tests passed (after the merge of the settled contract of p12-cp, `22a40eb52f`: dryRun preview, typed blockers, alert subject members, sentence step names, asOf bound, put refused during a run; mutants for these were added and caught). New: `analytics-lib-0-18` (46), `analytics-mock-0-18` (49), `analytics-ui-0-18` (65). One existing test, in `hostile.test.ts`, asked for "the table" of the database page and now names it, because the page has a second table (the servers of the card). `tables-0-14-mock.test.ts` compares `partitioning` with `toMatchObject` now, because 0.18.0 adds members to it. The full run once showed `task-notices.test.ts` timing out under load; it passes alone and in the later full run.
- `npm run test:browser` (Chrome): 4 files, 48 tests passed. The new file runs the eight states of phase 12 through axe in light and dark at 1366 and 320 px (no violation) and checks that no page scrolls sideways and that each dialog and its buttons stay inside the window at 1366 and 320 px.
- Mutants, each caught and reverted (the tree was clean after each): a new or shorter retention no longer needs the name typed; a missing ratio shown as `0.0 x`; a repartitioned join read as a colocated one (survived at first because the Repartition line gave the same answer; a test of the Pushed line alone was added, then caught); the mock accepts a columnar_after longer than the retention; the mock does not refuse a conversion for its reasons; the mock plan changes the partitions; a blocked storage change can still start; saving a retention does not ask for the name; saving a retention needs no preview; a preview of an older form is taken for the current one; the error rate of the dialog is not checked; a conversion of a partition does not hold the table page; the Storage filter shows every table; the plan is audited or needs admin (role change); a server without the sketch package is not refused; a hidden character of a server id shown as it is; in the browser: a select without a label (axe), a 900 px wide grid at 320 px (layout), an unnamed button (axe).
- `git diff --check`, the scan for the project name misspelt with a double s, and the scan for local paths in the changed files: clean.

### Decisions and deviations

- The Storage filter is made in the panel from `Table.storage`; the server's `storage` parameter is implemented in the mock but not sent (the whole list is read anyway).
- `Convert storage` is not offered for a distributed table or for the partitions of one (the release has heap distributed partitions only; D1b is later).
- The alert link reads the table from `AlertSubject.tableSchema` and `tableName` (settled in the contract after the first version of this entry, which read it from `aspect`).
- The mock uses the sentence step names of the settled README for `table.setStorage` and `partition.maintain`, and a disable of approximate aggregates runs the same five steps as an enable.

### Still open

- The contract notes (11 items) for the writer of the control plane.
- No install action for `kingres_columnar` (no operation for it); no preview for a storage change; the alert subject of the partition alerts; the distributed tables.
- The end-to-end run against a real `kingresd` once the control plane of phase 12 is built.
- Not pushed.

### Core patches

none

---

### Core patches

None.

---

## 2026-11-22: the fixes for the first independent review of phase 13 (configuration from the console)

### What was done

- The review ([reviews/2026-11-22-config-phase13.md](reviews/2026-11-22-config-phase13.md)) found 2 high, 5 medium and 8 low findings. All 15 are fixed or answered on
  branch `p13-fix` (made from `p13-fin`, contract 0.17.0, which is unreleased and was changed in place); the report has a "Fixes" section that maps each finding to its commit and test.
- High: the parameters that change how the sessions of `kingresd` itself behave (`default_transaction_read_only`, timeouts, `search_path`, ...) are refused at every
  level; `unix_socket_permissions`, `unix_socket_group`, `huge_pages`, every `ssl*` and others are managed or refused; a restart is checked with `postgres -C` first, and a restart
  that cannot start puts the last good override file back and starts the instance again.
- Medium: a server that is down no longer keeps the apply task open (it is recorded as `waiting` and the convergence loop brings it up to date); the hidden set fails closed;
  a stored value whose name became managed or refused is skipped and can always be removed; the restart goes through the instance lifecycle of the operations code and finds the
  primary of each group from the leader's record; the time-zone tests use a zone that is not the host's.
- Low: tests for the executed restart order, leading zeros and the audit; the audit details; the task codes; the reserved-connections warning; `expectedRevision` in the contract; the
  console sends it. Contract 0.17.0 gained the optional `expectedRevision` of `ConfigChangeRequest` (additive).

### How it was verified

- Every fix has a test that fails without it; each was checked with a mutant (listed in the report): 10 cluster mutants and 13 unit mutants, all killed.
- PostgreSQL 18 and 19 (private installs, lane l5): the `pgconfig`, `instance`, `httpapi` and `ops` packages, and the whole `TestConfig*` set of the cluster tests (23 tests) on three servers.
- Console: typecheck, lint, the whole unit suite (90 files, 1462 tests), the browser tests (layout, CSP, axe), `api:check`; `node kingres/api/scripts/check-contract.mjs` holds.

### Decisions and deviations

- The apply task no longer waits for a server that is down (design "Failure cases" changed). Audit details carry `basedOnRevision` and the task id instead of the revision, which is allocated after the audit event.
- `log_line_prefix` and `log_error_verbosity` stay settable (the console does not parse log lines). The console mock still models a waiting task for a server that is down.

### Still open

See the report: no cluster test with a promoted standby (unit test of the order only), no test that induces a failure of the hidden-set read inside `State`, the derived default of a removed
override is not known to the preview warning, and the mock of the console still models the old waiting task.

## 2026-11-22 (second entry): fixes for the independent review of the phase 12 control plane (P12C-01 to P12C-16), and the console's contract notes

Branch `p12-cp`. The review is [reviews/2026-11-22-control-plane-phase12.md](reviews/2026-11-22-control-plane-phase12.md); its "Fixes" section lists every finding with the change and the test.

### What was done

- The four high findings: the time of a run is the time of the server (within five minutes, for a request and for the scheduler), a run is always closed with `timepart_finish_run` (begin failure, step failure, the hook of the kind) so that a failing
  policy does not start a task at every tick, `kingres_columnar` goes into `shared_preload_libraries` only when the installation has the library (a rewrite is logged and reloaded, the conversion needs the library on every server), and the conversion and the runs share
  one rule for the owners of tables (`trust_table_owners` on, documented).
- The mediums and lows: columnar options on a columnar table, lock waits with `lock_timeout` and retry, a missing run is an error, alerts of databases that were not read are left alone, a scheduled run does not need the library, reads never install an extension,
  the limit to the leader's instance is in the contract text and the 404, `put` during a run, `Engine.ActiveFor`, versions as numbers.
- Contract text and shapes for the console (still 0.18.0): `dryRun` and `TableStoragePreview`, typed `blockers` on `conversion_refused`, `tableSchema` and `tableName` of `AlertSubject`, the shapes of `partitionEvery`, the bound of `asOf`; listed in `p12cp/contract-changes.txt`.

### How it was verified

PostgreSQL 18 and 19.0 (the private installations were refreshed from `~/kw-int/pg`, extensions built from e279c14091), lane l1: the PG-backed tests of `internal/ops`, `internal/store/pgstore`, `internal/instance` and the two cluster tests of three servers
(`TestApproximateAggregates`, `TestTableStorageAndPartitionPolicies`). Unit tests of `ops`, `partsched`, `task`, `monitor`, `api`, `problem`, `contract`; `check-contract.mjs`; `vue-tsc`. `httpapi` has one test that fails now and then on Windows on the cleanup of its temporary
directory (`TestSettingsOperations`), not on an assertion. The mutants of the round are in the report.

### Decisions and deviations

- The sound rule for the owners of tables is trust, as the extension offers it: a refusal of non-superuser owners (or a cluster setting) was not built; the contract text says what the setting gives away. It is a question for the project owner.
- The restart order of the preload change is the operator's: the restart of the console orders standbys and primaries by the rule of the configuration design, which was written for settings that go up; for a list of libraries the safe order (standbys first) is documented, not enforced.

### Still open

- A cluster test that restarts one server with the library missing (the logic is tested with a fake installation).
- The cluster setting for non-superuser owners, if wanted.

### Core patches

None.

---

## 2026-11-22: the table page and the table dialogs of the console (phase 9, increment 9.7) moved onto the control plane of 9.6

## 2026-11-22: the console of phase 14a, table CSV export and import (increments CN1 to CN5, contract 0.19.0)

Branch `dx-web`, from the contract commit cb4afe3757 of branch `dx-cp`. The control plane is written on `dx-cp` by another writer; this
entry covers the console only. No Go code and no contract text was changed.

### What was done

- **CN1, API layer and mock.** The ten operations in `endpoints.ts` and `queries.ts`; calls that keep the session in place on a
  401 (`unwrap(..., { keepSession: true })`) for the uploader; the texts of the twelve new problem codes; the labels of the task
  kinds `table.export` and `table.import` and of the eight new audit actions (the red tests A0 left); a mock that behaves like
  the server (offsets, 409 with the right offset, 64 KiB alignment, `Range` and 206, 416, 410, a 401 in the middle of an upload,
  quota, the setting off, the role, creator-only files) with the fixtures of the design and a real SHA-256, and the `?transfer=` switches.
- **CN2.** The *Data transfer* group of the settings page; the Transfers panel (export files and the uploads of the tab, Download,
  Delete) on the databases page and on a database page; the resumable download (`lib/downloader.ts`).
- **CN3.** *Export CSV* in the tables list: gates, consistency sentence before the confirmation, the spreadsheet text, options,
  acceptance of code of another role, task progress, result with the SHA-256, the sentence about exporting twice, the download.
- **CN4.** *Import CSV*: local preview and guess, the limit shown before the upload, options, the uploader (`Blob.slice`, 64 MiB,
  re-read offset, password on a 401, pause, resume, cancel, `sessionStorage` in `try/catch`), the import task, the whitelisted
  errors and the wording of `import_outcome_unknown`.
- **CN5.** Tests, fifteen browser states, mutants (below), README and this entry.

### How it was verified

- Typecheck, lint (eslint with no warning, prettier), `npm run api:check`.
- The full vitest suite: 97 files, 1580 tests, all passed.
- The browser tests (`npm run test:browser`, 12 minutes): 3 files, 49 tests, all passed. They include the axe run of fourteen states of the data transfer (the settings group, the panel with files and with the setting off, the export dialog and its refusal, its result and its failure, the import dialog with the preview, the options, the stop for the password, the refused size, a failed import and an unknown outcome) in light at 1366 px and dark at 320 px, and the layout run of the same states at 1366 and 320 px.
- **Mutants of the console** (a defect put in by hand, the tests that must notice it run, the file restored): 31 of 31 killed.
  They cover the downloader (a short file called complete, the ETag, the SHA-256, a resume from byte 0, `Content-Range`, more bytes
  than the record), the uploader (a conflict taken from the client, no re-read after a break, the 2 GiB cap missing or applied to a
  compressed file, a 401 as a failure, no session check between chunks, another size continued, cancel without delete, an unaligned
  chunk), the dialogs (the limit as `>=`, a repeat offered after an unknown outcome, the upload dropped after a failure), the
  options (the null limit, `skipBom`, the spreadsheet guard, the consistency sentence), the gates and the panel, the settings
  (sizes always sent), and the mock (another role sees a file, the alignment, a range past the end, a value in the failure text).

### The review of the console and its fixes

The independent review ([reviews/2026-11-22-console-data-transfer-14a.md](reviews/2026-11-22-console-data-transfer-14a.md)) found no critical or
high finding, eight medium (DXW-01 to DXW-08) and nine low ones. All are fixed with tests, with the tip of `dx-cp` (97976b3c44) merged: a conflict
that does not move the offset waits and stops as busy; a cancel whose delete was refused keeps the upload listed and says so; an import
repeat after an unknown outcome needs a tick; the cap of a reference table (256 MiB) is known before the upload; a failing file is a
save error and never hangs the dialog; a download that loses its session asks for the password and goes on from its bytes; the link
fallback shows the size and the SHA-256 and the commands to check it and is for administrators only; a 400 that carries the offset
resumes; a stream that fails after the last byte is complete; a download stops when the session of the console ends; uploads are
kept per person with a fingerprint of the file; a file that cannot be previewed gets no default for its first line. 37 mutants of the
fixed code were killed. The two contract requests of the review (the spreadsheet option and the effective import cap) were answered in contract 0.19.0
(`DataExport.spreadsheetSafe`, `DataTransferSettings.maxAtomicBytes`) and the console uses them: the download dialog warns about a file
written with the guard off, and the cap of one import comes from the server (a reference table stays at 256 MiB). Both are done.
The transfer actions are also in the header of the table page of phase 9.7 (branch `web-15b` merged).

### Decisions and deviations

- The download is not only the plain link of the design: a fetch with `Range` resumes after a break and reports a short file; the
  link is the fallback for a large file in a browser that cannot write to a file as it goes.
- A file kept in memory is checked against the SHA-256 of the export before it is handed over; a mismatch saves nothing.
- The 2 GiB cap is enforced in the browser before the first byte for an uncompressed file; for a gzip file the server counts after
  expansion, so only the largest staged file applies.
- The actions are mounted in the tables list of a database; the table page of phase 9 (branch `web-15b`) is not in this base and
  takes `TableTransferActions` in its header.

### Still open

- The progress indicators that need `Table.operation`; distribute with related tables.
- Not pushed.

---

## 2026-11-22: phase 12, the sharding extension: the fixes of the independent review of 0.9 (P12S-01 to P12S-06)

The review is in [reviews/2026-11-22-kingres-shard-0.9-storage.md](reviews/2026-11-22-kingres-shard-0.9-storage.md), with a Fixes section.

### What was done

- **P12S-01** the sketch is looked up only for a rewritable count(DISTINCT) and only when the role has USAGE on its schema and EXECUTE on the aggregate; otherwise the statement is exact with one warning.
- **P12S-02** both settings drop the cached plans when they change.
- **P12S-03** the options of columnar_set_options are carried by distribution, rebuilds, undistribute and moves (the bucket partition and the leaves).
- **P12S-04** the confirmation after PREPARE is skipped only for the node that add_node is adding.
- **P12S-05** every relation of a bucket is checked, not the first (a second table access method over the heap handler makes it testable).
- **P12S-06** tests for the oid types and for one shard group.

### How it was verified

- A new or extended test for each, each with a one-line mutant that it kills (t/270, 271, 273, 274, 275).
- The full suite on both majors is in the Gate section below.

## 2026-11-22: phase 12, the sharding extension: columnar tables in kingres_shard 0.9, the explicit refusal of ledger tables, and approximate count(DISTINCT)

Covers branch p12-storage (from p12f-fix, kingres_shard 0.8 with the review fixes of step 12).  Version 0.9 is new: the other writer of kingres_shard (shard-p12) adds no SQL object and stays at 0.8, so the integrator merges the two and resolves the version.

### What was done

- **Version 0.9.**  `kingres_shard--0.8--0.9.sql`, the generated `kingres_shard--0.9.sql` (the 0.8 install script followed by the upgrade script; `mk-install-script.sh` now writes both scripts and `t/250` checks both), the control file, `Makefile`, `meson.build`, the C version
  string, the version handshake (`internal_check_node_versions` asks for 0.9) and the `upgrade` regression test (the step from 0.8 on its own, and 0.9 at the end of every chain).
- **St-S1, the access method is carried** (`t/270`).  `internal_table_ddl` names the access method of the source (`USING columnar`) in the statement that makes the table and its shadow, so `create_distributed_table`, the blocking rebuilds,
  `undistribute_table` and `add_node` keep it; every bucket partition and leaf that is not heap gets `REPLICA IDENTITY FULL`; a user-partitioned table keeps the access method of each leaf (`internal_partition_layout` has an `am` column); the report
  gains `access_method_missing`, `columnar_reference_table` (a reference table stays heap, design 9.5) and the note `columnar_storage` (the 0.8 report is kept as `internal_table_change_report_base` and the 0.9 report calls it).  A defect found on the
  way: `add_node` onto a node that already had `kingres_columnar` or `kingres_ledger` failed after `PREPARE` ("function internal_sync_confirm does not exist": the schema `kingres` exists there and the function does not, which the check accepted only
  for a missing schema).
- **St-S2, refusals** (`t/271`).  The online variants of the lifecycle capture changes with a row trigger, which columnar does not support: the report has the blocker `columnar_online`, so the call is refused before an operation is recorded.  The split
  refusal has the words of the design.  `kingres_ledger` rule L1 (`t/040_columnar_refused` in kingres_ledger): `ledger_enable` refuses a table whose access method is not heap, in C inside `ledger_lock_table`, so kingres_ledger stays at 0.3; `SET ACCESS METHOD`
  on a ledger table is rejected by the existing rewrite trigger (the test shows it).
- **Phase 7 step 25** (`t/272`).  A ledger table, or the history table of one, is refused by `create_distributed_table`, `create_reference_table` and their online variants with SQLSTATE `KS016` and a message of its own (the detail lists every blocker with its
  code); `check_cluster` reports `ledger_distributed_table` for a ledger table that the catalog distributes or makes a reference table.
- **Q-S5, `count_distinct_mode`** (`t/273`).  `kingres_shard.count_distinct_mode` (`exact` or `approximate`) and `count_distinct_error_rate`; the planner hook rewrites `count(DISTINCT x)` of every level that reads a hash-distributed table into
  `kingres_sketch.approx_count_distinct(x, p)`; the new file `src/count_distinct.c`, one call in `planner.c`, `kingres_shard_extension_usable` in `shippable.c`.
- **St-S3, bucket moves of columnar relations** (`t/274`, `t/271`).  The copy is made `USING` the access method of the source, a leaf that is not heap has `REPLICA IDENTITY FULL`, the move works with `kingres_columnar` 0.2 or later and is refused with the words of
  the design with 0.1; inserts during a move, the move back, and a user-partitioned bucket with a columnar and a heap leaf are tested.

### How it was verified

- Every behaviour has a TAP test that fails without it; each was checked with a one-line mutant (clean build with -Werror against PostgreSQL 19 and the test run):  S1 seven, S2 four, rule L1 one, step 25 five, Q-S5 eleven, S3 four, every one killed except one (and the mutants that did not build or did not apply were redone as others):
  the check `get_typtype(base) != TYPTYPE_BASE` in `count_distinct.c` is redundant with the check of the extended hash function (a record has none in the type cache), so no test can tell the two apart.
- The full suites are recorded in the section below ("Gate").

### Gate of the fixes

- `kingres_shard` with the fixes, in a fresh area with PostgreSQL 19.0 and 18 (clean -Werror builds with no warning, regress 23 of 23, every TAP file): 224 files, 48556 tests on 18 and 49050 on 19.  One file failed in that run, `t/273`: a statement of the new assertion read a table the role may not read (a mistake of the test); fixed, and `t/273` passes alone on both majors from a clean build.  Every other file passed.
- The mutants of the fixes: fifteen, all killed (the first one that survived, the eager lookup, got an assertion).


### Decisions and deviations

- **Reference tables are not columnar** (design 9.5 recommends it; the S1 row of the plan says "carry"): `create_reference_table` refuses a columnar table with `columnar_reference_table`.
- **The online variants are refused for columnar**, not made to work: their capture trigger is a row-level AFTER trigger.  The blocking variants work.
- **The compaction before the switch (S3) is not done.**  The move would have to call `columnar_compact` inside `move_bucket`, a 450-line function; the stripes of the initial copy are full, only the changes of the move make small stripes.  The README says to run
  `kingres.columnar_compact` afterwards.  The options of `columnar_set_options` are not carried to the copy either.
- **The refusal for old kingres_columnar** is made by version (0.2 or later), so the refusal of S2 became the check of S3 rather than a second code path.
- **L1 is in C**, so the ledger extension needs no new version.  The other half of L1 (`convert_partition` and `columnar_*` refuse a ledger table) belongs to kingres_timepart and kingres_columnar and is not done here.
- **`EXPLAIN` does not print the line `Rewritten: count(DISTINCT x) -> approximate (p = 14)`** of the design; the aggregate shows as `approx_count_distinct(...)`.
- The rewrite runs before the shortcut for a cluster of one shard group, so an answer does not depend on the number of groups.

### Still open

- The control plane and console parts of phase 12 (Q-C1, Q-W1, the storage and partitioning operations) are not in this branch.
- Columnar tables in a distributed schema (phase 11) are not tested: the shells are made with the access method now (`internal_table_ddl` is shared), which needs `kingres_columnar` on every node.
- Moves of a schema (`move_schema`) were not exercised with a columnar table.
- Merge: `kingres_shard.h` (the version define), `Makefile` and `meson.build` (data files, objects), `mk-install-script.sh`, the README, `planner.c` (one call), `shippable.c` (one function at the end) and `node_conn.c` (one condition) are the files that the shard-p12 branch may also change.

### Gate

- `kingres_shard` at the tip before the last three fixes (clean -Werror builds with no warning, regress 23 of 23, every TAP file) on PostgreSQL 18 and 19: 223 files, 47924 tests on 18 and 48436 on 19.  Eight files failed in that run, each for a reason that was then fixed and the files run again on the fixed tree on both
  majors (10 files, all passing): six tests that name the version of the extension in an expected message (t/036, 047, 058, 105, 140, 148 ask for 0.9 now), `t/098` (no file but the lookup names the extended hash function: `count_distinct.c` asks the type cache for the finfo), and `t/272` (a regclass printed with and without its schema).
  An earlier gate of the S1 commit alone had the same version failures and one failure of `t/085` on 18 that did not repeat (a wait for a WAL segment; it passes alone).
- `kingres_ledger` (regress and 39 TAP files, 2587 tests) on 18 and 19 with the check of rule L1: all pass.

- E1 (the console against a real `kingresd`), after CP3, CP5 and CN4.
- A browser test that downloads a file (headless Chrome would open the file picker).

### Core patches

None.

## 2026-11-22 (later): fixes of the independent review of phase 14a

### What was done

- The review (0 critical, 1 high, 4 medium, 7 low) is in `docs/reviews/2026-11-22-data-export-import-14a.md` with a section "Fixes". The high finding (the commit intent
  was overwritten after COMMIT, so a re-run said "nothing stored" and a retry doubled the rows) is fixed by a marker row that commits with the data; load and commit are one
  step. The mediums: the staged files are adopted at start (quota), a dangerous role is refused for import and the owner test of the export no longer trusts a superuser, the
  header cells are guarded, the checks of the export are repeated at step time. The lows: no `lost` mark from a poll, the size cap reads the file, the column types are compared,
  `skipBom` only for UTF-8, xml guarded, the janitor re-reads before its alarm, one transfer per role without a race, refused requests leave no record.
- Contract 0.19.0 texts only (steps, the 403 and 400 cases, `import_outcome_unknown` now rare); logged in `contract-changes.txt`.

### How it was verified

- New tests (`review14_e2e_test.go`, `review14_restart_e2e_test.go`, `TestImportCrashAfterCommit`) on PostgreSQL 18 and 19; each was shown to fail with its fix removed (mutants:
  header guard, owner test, dangerous role, skipBom, types, lock, adoption, setting, record removal, gzip look, marker read, `row_security`, untrusted check).

### Still open

- The xml guard cannot be run here (the test build of PostgreSQL has no libxml); the janitor retry has no test that fails without it (the race needs a hook inside the sweep).
- A real failover of the leader at every step and a shard group lost exactly at COMMIT, as before.

---

## 2026-11-22: data export and import of tables (phase 14a, control plane): contract 0.19.0, the relay, uploads, downloads, the export and the import

### What was done

- **Contract 0.19.0** (cb4afe3757): ten operations, the vendor extension `x-streams`, the setting `dataTransfer`, twelve problem codes, two task kinds, the
  checker rules and the generated files; two texts corrected later (validation_failed is 400, insufficient_disk 409).
- **CP1:** the table `kingres_cp.data_transfer` (migration 0035), the setting, the flags, the check of the role between steps, `transfer.SafeError` with the canary and
  fuzz tests. **CP2** came from the second writer (`internal/staging`, merged). **S1:** the relay spike (5 GiB each way, heap growth 3 and 13 MiB; it found that the idle
  body must move the write deadline too). **CP3:** `POST /internal/v1/forward-stream`, the guards (TLS, the setting), uploads, downloads with Range, the janitor.
  **CP4:** the task `table.export`. **CP5:** the task `table.import` (all or nothing, the commit intent, reference tables through typed arrays, S2 applied).
- The design note has a section with the spike results and every place where the code differs from the text.

### How it was verified

- Unit tests of the relay, the internal call against a real mutual TLS listener, the guards and the relayed operations, SafeError (canary in every class, fuzz), the scanner
  (against a reference, fuzz), the options and statements; PostgreSQL 18 and 19: the record and the setting, uploads, exports, downloads, imports of every kind of table, the
  error classes with a canary, the commit fault points, cancel, demotion; three servers: uploads, import, export and download through followers, a follower that dies
  in the middle of an upload.
- Mutants killed: SafeError copying the message; the body limit on the relay call; the proof check of the relay; the TLS guard; the setting guard; the stream limits and
  the relay in the guard; the chunk timeout; four of the scanner (quote toggle, record limit, the end-marker check; the fourth was not confirmed); the count comparison
  of the import.

### Still open

- The independent review of the relay and of the import transaction, the leader-change fault tests of a real failover at every step, the console (second writer), the batches (14a+).
## 2026-11-22 (later): the search path of the service connections (DX16-01, branch sp-fix)

### What was done

- A security review found that a service connection set no search_path: the owner of a database can run `ALTER DATABASE ... SET search_path = <own schema>, pg_catalog`, plant an
  operator of the same signature in that schema, and the service role (a superuser) resolves an unqualified comparison of the control plane to it and runs its function. The three
  places that open a service connection (`Service.connect`, `InstanceConn`, `RemoteConn`) now set `search_path=pg_catalog` in the startup parameters, which outrank the settings of a
  database and of a role, and `asService` repeats it with `SET LOCAL` as the first statement of every transaction. The control plane was audited by running the whole end-to-end
  suite with the path: no statement relied on another schema.

### How it was verified

- `TestServiceSessionIgnoresTheSearchPathOfTheDatabase` plants the schema, the operator (regclass, regclass) and the database setting, then runs a distribution of a reference table (the
  check compares two regclass values): without the fix a role `kg_sp_pwned` with SUPERUSER exists afterwards, with it none. PostgreSQL 18 and 19: the whole `cmd/kingresd` suite passes;
  the first 35 tests of `internal/clustertest` (failover, standbys) passed on 18 before the run was stopped for time.

### Still open

- The connections that the sharding extension opens to other nodes are not audited here.

## 2026-11-22: the table page and the table dialogs of the console (phase 9, increment 9.7) moved onto the control plane of 9.6

Branch web-15b (from m18) carries the one console commit of web-15 for increment 9.7, cherry-picked so that the history stays one commit. web-15 also holds the failover console of contract 0.15.0, which m18 does not have; it is not taken here.

### What was done

- The table page `/databases/<name>/tables/<schema>/<table>`, the dialogs distribute, undistribute, change the distribution, create, attach, detach and drop a partition, the preview panel and the task panel, from web-15.
- The conflicts (README, browser harness, `mocks/http.ts`, `mocks/state.ts`) all came from the 0.15.0 failover console; only the 0.14.0 table parts were kept. The fixed-shape mock handlers that 9.6 put on m18 for the eight operations were replaced by the mock of 9.7 (`src/mocks/tables.ts`); the generated types, operations and problem codes are those of m18, and no stand-in type is left. A doc comment that had slipped above a new constant in `handlers.ts` was put back.
- Checked against shard-phase9.md: each of the eight operations of 9.6 has its UI (the preview in every dialog, undistribute, alter, create, attach, detach, drop, the list of partitions). Not done as the design describes: the progress indicator on a row of the tables list and the badge on the Shards page need an `operation` field on `Table`, which contract 0.14.0 does not have; "distribute with related tables" (control plane and dialog) stays open, as the README says. The reviews in kingres/docs/reviews say nothing about the console of 9.7 (the review of increments 4 to 9 lists the console as not in the commits).

### How it was checked

- `npm run typecheck`, `npm run lint` (eslint and prettier) and `npm run api:check`: clean.
- `npx vitest run`: 92 files, 1513 tests passed.
- `npm run test:browser` (Chrome): 3 files, 46 tests passed (axe in light and dark at 1366 and 320 px, layout, content security policy), including the table route and the four table states.
- Mutants, each caught by the tests and reverted: a blocked change no longer holds the start; the partition name limit raised by 100 bytes; "Cancel the task" shown for a task that cannot be cancelled.
- `go test ./internal/...` on Windows: every package passes except `internal/pgconninfo`, whose test binary Windows refuses to run ("Access is denied", a build failure, repeated). No Go file is changed here. `internalapi` and `ledgersign` failed once under the load of the browser run and passed alone.

### The review of the console (2026-11-22) and its fixes

The independent review (docs/reviews/2026-11-22-console-phase9-7.md, no critical or high finding) had seven findings, all fixed in a second commit on web-15b: type-to-confirm and the danger tone for undistribute and alter (C97R-001); no claim that a cancelled task left the table as it was, the detail of the task shown and read again after a cancel (C97R-002); the mock switches blockme, diffme, pending, undo:<ms> and undo:fail with tests of the paused, verification-failed and pending states (C97R-003); a test that the start waits for the preview (C97R-004); a name on every group of radios (C97R-005); a test of the 57 bytes with two-byte letters (C97R-006); pending tasks hold the buttons and the page reads 200 tasks (C97R-007). The runs and the mutants are in the Fixes section of the review and in the final report of the work.

### Still open

- The progress indicators that need `Table.operation`; distribute with related tables.
- Not pushed.

---




## 2026-11-23: the review of the CA key escrow and its fixes (branch ca-escrow)

The independent review (`docs/reviews/2026-11-22-ca-key-escrow.md`) found the cryptography sound and eight findings about state and failover (one high, four medium, three low).

### What was done

- **CAE-4 (high)**: a revocation list never shrinks. Holders refuse a list that takes a revocation back (409 `stale_revocations`); a server that holds `ca.key` merges the lists it receives into its CA; a leader that takes over or distributes merges the lists of its files, its sealed copy and every server first; the list number is at least the time in seconds.
- **CAE-1**: copies are ordered by (epoch, generation, seq); a new leader takes a higher epoch; recipients refuse a blob and a drop of an older epoch.
- **CAE-2, CAE-3, CAE-6**: records stay (`dropPending`) until a drop succeeded; the operator's choice, the epoch and the holders are mirrored to `kingres_cp.cluster_setting` (`caEscrow`); a standby missing for less than three rounds keeps its copy; switching off tells every active member.
- **CAE-5, CAE-7, CAE-8**: durable write of `ca.key`, a truncated key is put aside, the list in a blob is parsed on store, length-prefixed additional data, key file mode check, clearing of secrets where Go allows.
- The cluster test of CAE-4 found that every revocation task on the new leader failed after a failover of the map group (the row of the server that created the cluster has no address); the task now takes the ids of the active members and leaves the address to the peer client.
- Contract 0.26.0 gets the value `dropPending` of `CaEscrowRecipient.state`; the internal calls get `epoch`, `heldEpoch` and `heldGeneration`.

### How it was verified

Unit tests of pki, membership, httpapi, internalapi and kingresd; mutants for each fix (listed in the Fixes section of the report); cluster tests on PostgreSQL 18 and 19 (lane l7), among them `TestCAEscrowARevocationAfterTheFailoverHoldsWhenTheOldLeaderReturns`.

### Still open

The `ca.key` of a returning former leader stays on its disk; a removed server that cannot be reached keeps its copy (`dropPending`); a console panel; an alert for a recipient that stays out of date; the rotation of the CA itself.

---

## 2026-11-22: phase 5, the escrow of the key of the certificate authority (branch ca-escrow)

The last open item of phase 5: after a failover of the leader's server, the new leader raised a critical alert until the operator restored `pki/ca.key` by hand.

### What was done

- **The design** is in `docs/design/ha-phase5-part2.md`, section 3.4.1: the alternatives (a copy wrapped to each server that may become leader; a key from an operator passphrase;
  split shares; a blob in the system database), the choice of the first, the construction (X25519 key agreement, HKDF-SHA256, AES-256-GCM, additional data that bind the recipient,
  key id, CA fingerprint, generation and time), the checks of a recipient and of the adoption, and the threat model. The first design (a key derived from the cluster secret) was
  not buildable: the secret lives in the extension, not in `kingresd`.
- **The control plane**: `internal/pki/escrow.go` (escrow keys, seal, open, store, drop, adopt, the leader's bookkeeping), `internal/membership/escrow.go` (recipients from the standbys of group 0,
  the minute-by-minute reconcile and the resend after every revocation, the three internal calls `POST /pki/escrow/offer`, `store` and `drop`, all `LeaderOnly`), the adoption at B8 through the
  port `CheckAuthority` (`cmd/kingresd/mapfailover.go`), the text of the alert.
- **The operator path**: contract 0.26.0: `getCaEscrow`, `setCaEscrow`, `rotateCaEscrow`, the schemas `CaEscrowStatus`, `CaEscrowRecipient`,
  `CaEscrowUpdate`, two audit actions. The escrow is off until switched on. The shapes are in the scratch file `ca-escrow/contract.txt` for the integrator. No console part (the operations are REST only).

### How it was verified

- Unit tests: `internal/pki/escrow_test.go`, `internal/membership/escrow_test.go`, `internal/httpapi/handlers_caescrow_test.go`; the contract check (`node api/scripts/check-contract.mjs`), the generated operation table and the
  README table test. Cluster tests (`internal/clustertest/caescrow_test.go`): the failover of the leader's server with the escrow on, and with a tampered copy. Results: both cluster tests pass on PostgreSQL 18 and 19 (lane l7, private installs of the extensions of 908518fa05).
  Mutants killed: a recipient or the adoption that skips the check against `ca.crt`, no replay check, no label check, no merge of the revocation list, an adoption over a held key, no pruning of old escrow keys,
  a content hash that ignores the revocation list, no drop of a server that is no longer a member, a recipient outside the standbys, no resend after a revocation, a non-leader that sends, escrow calls not `LeaderOnly`
  or not dispatched, and (cluster, 18) a `CheckAuthority` that does not adopt. The cluster test found one defect that no unit test had: the three calls were missing from `internalapi.handled` and answered an empty body.

### Decisions and deviations

- Off by default, because every recipient's disk then can hold the CA key (threat model, 3.4.1). Recipients: only the servers that host a ready standby of group 0.
- The alert `certificateAuthorityUnavailable` stays for a failover without a copy; no new alert kind was added.

### Still open

- A console panel for the three operations; an alert for a recipient that stays out of date; the removal of `ca.key` from an old leader that returns; the rotation of the CA itself.

---

## 2026-11-22: phase 11 (distributed schemas), the extension: the scale work (step 12), the review fixes and the documentation (step 15)

Covers the work on branch shard-p1011 after the second review of phase 11 (the review fixes of 2026-11-21 are in the entry of that day).

### What was done

- **Step 12, the scale work** (`contrib/kingres_shard`).  The measurements of `t/245` (2000 schemas of 5 tables) showed three growths and a failure; each is fixed and measured (design notes, "Step 12"):
  the catalog cache of a backend makes the entry of a schema table or schema when a statement asks (one index scan, "not a schema table" cached), and colocation groups are found through a hash;
  the trigger of a catalog change invalidates the tables it touches, not every table (`kingres_shard.catalog_incremental = off`, superuser only, is the fallback, and the third profile of the
  harness `KS_CATALOG_FULL=1` runs the suite with it); a node applies a whole catalog with one statement per table; `add_node` makes the schemas on the new node before it is registered, in
  batches of `kingres_shard.add_node_batch_statements` statements, resumable (`kingres.add_node_progress`), and registers it in one last transaction that checks completeness.  At 2000
  schemas: a DDL statement 52 ms (174), the first statement of a backend 9 to 15 ms (55 to 112), the cache of a backend 0.7 MB (3.3), a routed INSERT 1.2 ms (5.8), `add_node` 39 s in 500
  batches (it failed for lack of locks).
- **The review of step 12 (P12R-01 to P12R-07)**, fixed: `split_bucket(verify => 'full')` compares as text (a json, xml or point column has no equality operator, `t/249`); the generated
  `kingres_shard--0.8.sql` is checked against `mk-install-script.sh` (`t/250`) and was regenerated after the merge of `ext-partfix` (a fresh 0.8 install had its partition blocker back); the lazy
  lookups take the primary key index from the open table, not an OID kept at load (REINDEX CONCURRENTLY, a renamed or missing index, `t/247`); `add_node` cleans the new node when there is no
  schema left to make and a missing role is an error of the call (`t/251`); sha256 digests; `catalog_incremental` is `PGC_SUSET`.
- **The review of those fixes (P12F-01 to P12F-03)**, fixed: a catalog table of the extension that lost its primary key is read without an index and a warning is raised once, so that `ALTER TABLE ... ADD PRIMARY KEY` and every other statement still run (`t/247` E); the roles of the distributed schemas are checked on the new node with a query on `pg_roles` before staging and the staging session reads English messages (`t/251` B); `internal_split_switch` sets `extra_float_digits = 3` itself (`t/249`).  Review: `reviews/2026-11-22-kingres-shard-step12-review-fixes.md`.
- **Steps 13 and 14, the control plane and the panel, are not part of this branch.**  They were built here (contract 0.15.0, five operations, three task kinds, the task engine's `Step.Recover`, migration
  0050, the panel's dialogs) and then taken out again: the branch cp-1011 already carries the control plane and the console of phases 10 and 11 (contract 0.16.0, after failover 0.15.0 and cp-push
  0.14.0), in its fixed merge order.  The work stays on the branch `p11-cp-steps13-14` (8c9488e9aa) for a comparison with cp-1011.
- **Step 15, the documentation**: the section of the extension README ("Distributed schemas", with how to use them and the limits) and this entry.

### How it was verified

- `kingres_shard` at the tip with the extension changes (clean -Werror builds, regress, every TAP file) on PostgreSQL 18.7 and 19rc2: 48828 tests on 18 and 48794 on 19 (218 files each), in tree
  218 of 219 (the one skipped is `046_two_majors`, as always) and `kingres_ledger` 39 of 39.  The files that touch the catalog cache also pass with `KS_CATALOG_FULL=1` (31 files, 4317 tests on each
  major).  Those runs were made with the control plane and panel merged; they do not run any of their code.
- Mutants: step 12 (b) six, (a) seven, the apply of a catalog one, the review fixes six; every one is killed by a named test.

### Decisions and deviations

- The control plane and the panel of distributed schemas come from cp-1011.  What the work on `p11-cp-steps13-14` has that cp-1011 may lack (the exclusion of the schema tasks and a failover, `Step.Recover`
  for a NonIdempotent step, the detail that names keyless tables before a move) is for a later comparison.
- The extension's `check_cluster` has no schema checks of its own yet (design 9.10).
- The rebalance plan (`rebalance_plan()`) plans buckets only; it does not move a schema.

### Still open

- Independent review of the extension work of this entry.
- The comparison of `p11-cp-steps13-14` with cp-1011.
- A load that is incremental per colocation group, if 2000 schemas are not enough (the DDL statement still grows from 26 to 52 ms between 200 and 2000 schemas).
- A flake seen once under the control plane work: the switch of a move under a running writer timed out on a node and the move was undone; not understood.
## 2026-11-22 (third entry): fixes for the second review of the phase 12 control plane (P12D-01 to P12D-09)

Branch `p12-cp`. The review is [reviews/2026-11-22-control-plane-phase12-fixes.md](reviews/2026-11-22-control-plane-phase12-fixes.md); its "Fixes" section lists every finding with the change and the test.

### What was done

- A scheduled run is compared with the last run of its policy (a clock that stepped is refused, one alert); a failure before any run exists backs off on the policy itself; the library check covers standbys; the preload list keeps the library of an instance that had it.
- The decision for non-superuser owners: the cluster setting `analytics.allowNonSuperuserOwners` (default off, contract 0.18.0), the refusal codes, one alert instead of a retry, and the corrected trust text. `tableOwner` and `ownerTrusted` on the policy and the storage preview.
- Lows: reader stall shortened (200 ms) and described, alerts of dropped databases cleared, one alert per refusal, `maintainRunning` fails closed, back-off on the last run.
- Contract changes for the console are in `p12cp/contract-changes.txt` (items 14 to 17).

### How it was verified

PostgreSQL 18 and 19.0, lane l1: the PG-backed tests of `internal/ops`, `internal/instance`, `internal/store/pgstore` and the two cluster tests. Unit tests of `ops`, `partsched`, `monitor`, `api`, `contract`; `check-contract.mjs`. Mutants are in the report.

### Decisions and deviations

- The setting is a cluster setting read from `kingres_cp.cluster_setting` at each use; a system database without it means off.

### Still open

- A cluster test that restarts one server with the library missing.

## 2026-11-22: phase 12 control plane (contract 0.18.0): approximate aggregates, storage conversion, time-partitioning policies and their scheduler, and the preload of kingres_columnar

Branch `p12-cp` (worktree of its own), from `kingres-18` at e279c14091. Items St-P1 to St-P5 and Q-C1 of the inventory of 2026-11-22; the console parts (W1, W2, Q-W1)
are on `p12-web`.

### What was done

- **Contract 0.18.0** (own commit): 11 operations (`getApproximateAggregates`, `configureApproximateAggregates`, `setTableStorage`, `setPartitionStorage`, `getPartitionPolicy`,
  `putPartitionPolicy`, `deletePartitionPolicy`, `runPartitionPolicy`, `listPartitionPolicyRuns`, `getPartitionPolicyRun`, `planPartitionPolicy`), three task kinds, six problem codes, two alert
  kinds, six audit actions, the optional `storage` and `partitioning` members of `Table`, the query parameter `storage`. `listTablePartitions` (0.14.0) is extended, not duplicated. Numbered 0.18.0
  because the other branches take 0.15.0 to 0.17.0; the integrator renumbers (and the migrations 0035 and 0036). The generated files, the labels and the mock statuses of the panel follow.
- **The release blocker**: `instance.PreloadLibraries` is `kingres_shard,kingres_ledger,kingres_columnar`; `EnsureConfig` brings an older `kingres-managed.conf` up to date at the start of `kingresd`;
  `kingres_timepart` and `kingres_sketch` need no preload (read in their READMEs and tested). The flow test and the text of the packaging README follow.
- **Q-C1** `database.approximateAggregates` (five steps, one install step per server), migration 0035, the closed `apply` kinds `extension.check`, `extension.install`, `extension.verify`, `database.settings`,
  and a read of another server through the internal call `exec` (`membership.Manager.Query`).
- **St-P2** `storage` and `partitioning` in `listTables`/`getTable` (pgstore), the filter, `listTablePartitions` for ordinary partitioned tables, the install of `kingres_columnar` and `kingres_timepart` in every
  database that is made (and, through the existing copy of the extension list, on every server that is added).
- **St-P3** `table.setStorage` for a table and for a partition. **St-P4** the policy resources. **St-P5** `internal/partsched`, `partition.maintain`, `runPartitionPolicy`, the runs, the alerts and the
  counters on `/metrics`; migration 0036.
- Documentation: `kingres/control-plane/README.md` (section "Analytics"), `kingres/api/README.md` (changelog 0.18.0, task kinds), `internal-api.md`, `kingres/packaging/README.md`.

### How it was verified

See the tests named in the README section. On PostgreSQL 18 and 19, lane l1, private installations built from e279c14091 (`install-extensions.sh` now builds the five extensions):
`TestApproximateAggregates`, `TestTableStorageAndPartitionPolicies` (cluster tests, three servers), `TestIntegrationSetStorage*`, `TestIntegrationPartitionPolicyResource`, `TestIntegrationMaintain*`,
`TestIntegrationPartitionScheduler*`, `TestIntegrationTableStorage`, and the tests of `internal/instance`. Unit: `go test ./internal/ops ./internal/partsched ./internal/httpapi ./internal/api ./internal/problem ./internal/store/...`,
`node scripts/check-contract.mjs`, `npm run api:check`, `vue-tsc`. Mutants: see the list in the report of the round.

### Decisions and deviations

- A database that was made before this version gets `kingres_columnar` and `kingres_timepart` at the first use of a storage or policy operation, where the design answers `extension_missing` for an extension that is not
  installed (nothing in the contract installs them). `extension_missing` stays for a package without the extension and for a library that is not preloaded.
- Storage and policy operations work on the instance of this server (where the leader runs). A plain table of another group's instance is not found; distributed tables are D1a and D1b.
- Step names are sentences (house style) and not the single words of the design table. `itemsDone` and `itemsTotal` of the convert step are not set (the environment of a step has no call for them yet).
- `createdBy` of a policy is the service role; the audit log has the role that asked.
- `database.approximateAggregates` is not in the exclusion rules of the task table (a failover does not stop it); the engine's one task per subject covers the second request.

### Still open

- Cluster test with a server whose package has another `kingres_sketch` version (all servers of the tests share one installation).
- D1a, D1b (policies and conversion on distributed tables), S3 and the console steps.
- Retention and conversion of the columnar partitions need `kingres_columnar` 0.2 in the packages of every server of a cluster: a mixed cluster is not checked by `partition.maintain`.

### Core patches

None.

---

## 2026-11-22: fixes for the review of the schema tasks port on cp-1011

### What was done

- The review of the three port commits (`reviews/2026-11-22-cp-1011-schema-tasks-port.md`, no critical or high finding) was
  fixed: `Recover` cut off by a shutdown no longer fails the task (SP1); the move step asks the extension to settle a move that no
  call drives, repeatedly, and has a bound on the fresh path (SP3); only the extension's refusal of a cancel is "too late" (SP4); the
  late-cancel note survives the clean-up step (SP5); the make steps accept a failed call that took effect (SP6); the web note for a
  failed move names only the cases where the schema stayed (SP7); partitions are checked for a replica identity (SP8); a make local
  that moved first says where the schema went (SP9); migration 0041 and the memory store also exclude `schema.move` and
  `schema.undistribute` against `server.add`, `server.remove` and `server.leave` (SP11).
- A make local with a move first could never finish within the cleaning delay; it now ends the cleaning of its own move first.
- Tests: kills inside the call of the extension (distribute, make local), the other direction of the rebalance exclusion at the
  request, a terminated move backend with and without the leader, unit tests for the engine and the guards.

### How it was verified

Unit tests of task, ops, httpapi, rebalance, sysdb and failover; the web unit tests (1484) and the browser test of the schema
dialogs; the trigger tests and `TestSchemaTasksOnACluster` and `TestDistributedSchemasThroughTheAPI` on PostgreSQL 18 and 19 (lane
l1); each new test against a mutant (the table in the review file).

### Still open

- A prepared transaction left by a cut `distribute_schema` cannot be made on demand (no fault point in the call): the test covers the
  branch of `Recover` and the meeting of the two calls, not the resolution of a real leftover.
- A make local whose schema has no row on the map group is taken as made, which is also true for a dropped schema.

---


## 2026-11-22: the open low finding and notes of the second look at the fixes of C13 (C14-08, C14-09, C14-10)

### What was done

- **kingres_columnar** (branch c14-low). C14-08: a developer gate (point 8 of `kingres_columnar.reserve_gate_at`) between the vacuum lock and the read of the ranges, and `t/070`
  stops the index vacuum there and checks that a writer's give-back is refused. C14-10: the skipped merge of a `VACUUM` that already has a transaction id says so at `DEBUG1`, with
  a test through a dropped table's orphan catalog rows. C14-09: the README states the cost of the refused give-back with its rate.

### How it was verified

- `t/070` passes; the mutants "lock after the ranges" (C14-08) and "message at DEBUG5" (C14-10) each fail it. The full suite of the extension runs on PostgreSQL 18 and 19.

### Still open

- Nothing from the second look at C13: C14-04 stays documented only, as the review says.

---


## 2026-11-20: automatic failover, the last steps of phase 2b and their review, the second review of phase 9 and its fixes, and two npm alerts

Covers 2026-11-20 from the morning, after the third entry of 2026-11-19.

### What was done

- **Phase 2b, steps 7 to 13** (2395ed9d98, d36744004b). Step 7 (partial
  aggregation) and the rest of the review of step 5, with a crash under
  `debug_discard_caches`. Steps 8 to 12: objects of extensions
  (`kingres_sketch`), sort and limit by the groups, the join of tables
  placed together done by the groups, the README for pushdown and a
  security round. They add no SQL object and ship in 0.7. An
  independent review of steps 6 to 12
  ([reviews/2026-11-20-kingres-shard-phase2b-steps-6-12.md](reviews/2026-11-20-kingres-shard-phase2b-steps-6-12.md))
  compared about 13 000 statements with a single node on both majors and
  found one critical finding, P2E-01: a `HAVING` condition without an
  aggregate was applied by nobody on the partial aggregation and the
  join rows. Those paths are refused for such a statement and the scan
  answers. The other findings are fixed as well: `TABLESAMPLE` of a
  distributed table is refused; the older tests also run with every path
  on; tests separate rules that no test separated; and the README is
  corrected. Step 13 adds the remaining tests of the plan, KS001 for a
  node that does not have an extension the statement uses, and a fixed
  order of ties by group id. One of its tests found a crash of a group
  (a prepared statement with a sketch aggregate, run again in the same
  session, read a list of the previous statement), which is fixed. The
  review of step 13
  ([reviews/2026-11-20-kingres-shard-phase2b-step-13.md](reviews/2026-11-20-kingres-shard-phase2b-step-13.md))
  found no wrong answer in 5120 generated statements per major and no
  critical or high defect. Its three medium findings are fixed on a
  branch and come with the next batch: a node that never had the
  extension got PostgreSQL's own error, not KS001; one test never reached
  the conversion it tested; the ledger test did not use the pushdown
  paths. Notes in [design/shard-phase2b.md](design/shard-phase2b.md).
- **Phase 9.** A second independent review
  ([reviews/2026-11-20-kingres-shard-phase9-rebuild.md](reviews/2026-11-20-kingres-shard-phase9-rebuild.md))
  of the online rebuild engine and of the fixes for the first review
  found that P9A-01 holds and found two high defects: a function of a
  role that is not a superuser, reached through a domain or an operator
  of the table, ran as a superuser during the copy; and the capture lost
  float keys when the writer's `extra_float_digits` was below 1. Four
  medium, four low and three notes. All are fixed with tests t/180 to
  t/184 (933025b5d0). Notes in [design/shard-phase9.md](design/shard-phase9.md).
- **Failover, increment 6** (1f868cc33d): automatic failover, off by
  default, with its rules and a counter. Two test faults found by a
  complete run (00bf7ef2f5).
- **npm alerts** (2480e640e6, 57bc832d6d): `minimatch` in the console's
  build tools raised to 11.x through an override, and `yaml` in the
  contract check raised to 2.8.3. Both alerts were moderate and in tools,
  not in what runs.

### How it was verified

Run by the integrator on the tree that was committed, unless it says
otherwise.

- `kingres_shard`, steps 9 and 10 (268a81d93f): 38413 tests on 18.7 and
  39148 on 19rc2 (155 files), in tree 198 of 198. The combined tree of
  steps 8 to 13, their fixes and the fixes of phase 9: 43317 tests on
  18.7 and 44022 on 19rc2 (169 files), in tree 212 of 212. The first run
  on 19 met another run's install of the extension in the shared
  PostgreSQL install and was repeated with a private copy; every run now
  installs into its own copy. The 2b
  writer ran the second profile (every path on) of the ten older tests
  on both majors; the phase 9 writer ran t/140 to t/150 and t/180 to
  t/184 on both.
- `kingresd` at 00bf7ef2f5 (gate7): a complete integration run on each
  major against the committed extensions. On 18, 1523 top-level tests
  passed and 2 failed; on 19, 1524 passed and 1 failed. The failover and
  takeover tests passed twice more on each major (24 of 24 each time).
  The three failures are faults of the tests: `TestClientEntryPoint`
  read through the entry point too soon after the cluster formed (seen
  once before; then 10 of 10 alone);
  `TestAReleaseWaitsForTheRenewalInFlight` waited 5 s for a renewal on a
  loaded machine (then 10 of 10 alone); and a failover test on 19 resumed a paused
  task at once after the heal, while `kingresd` read its fault file at
  most every 100 ms and still saw the pause. The same cause explains the
  timeouts "in two of twelve runs" of the last entry. Build for both
  systems, vet, format and 57 unit packages on Windows after increment 6.
- The console after the npm change: lint, the type check, the build and
  1455 tests; the contract check passes.

### Decisions and deviations

- **P2E-01 is fixed by refusal**, not by putting the moved conditions
  into the shipped `WHERE`, which would need `flatten_group_exprs` on
  both majors. `HAVING <grouping column>` is answered by the scan.
- **`TABLESAMPLE` of a distributed table is refused**: a sample spread
  over groups is a different random draw from a single node's, and
  `REPEATABLE` would not repeat.
- **KS001 for a missing extension is raised by the node that sent the
  fragment**, not by the group: PostgreSQL analyses the `DECLARE` before
  any hook of the extension.
- **P9B-01 is fixed by refusal**: a table that reaches code of a role
  that is not a superuser cannot be distributed by these calls, also
  when the function is `SECURITY DEFINER`.
- **P9B-11 stays**: the check after the switch cannot see a loss once the
  table is written after the swap; the call warns and the README says so.
- **The push of 2026-11-20** stopped at 54b3938a88, the last commit
  before phase 9, while the two high findings of phase 9 were open.

### Still open

- **Pushes.** This entry is pushed to kingres-18 with the commits it
  names, and kingres-19 is synchronized after it. Next batch: the fixes
  for the review of step 13 and phase 9 increment 9.4 (undistribute and
  alter, blocking and online), then switchover (failover increment 7,
  round 1) after a complete integration run of the control plane.
- **The console** is generated from contract 0.12.0; the contract is
  0.13.0, and switchover changes its text again. One round brings the
  console to the newest contract after the switchover work.
- **npm audit** of the console still lists a moderate advisory of `glob`
  whose only fix downgrades `@vue/test-utils`.
- **Reviews owed**: step 13 of phase 2b, failover increments 4 to 7, the
  publication and anchoring, `kingres_timepart` 0.2, columnar C5 to C8,
  the verifier's anchors, the console, packaging.
- **In progress**: switchover (failover increment 7, round 1 built),
  phase 9 increment 9.4 (blocking undistribute and alter built, online
  forms next), phase 7 steps 17 and 20.
- `kingres_columnar` uses `RM_EXPERIMENTAL_ID` for its WAL; a custom id
  must be reserved before a release.

### Core patches

None.

---

## 2026-11-19 (third entry): the review and the rebuild engine of phase 9, the fixes for the review of step 5 of phase 2b, the failover of the map group, the verifier's anchors, and columnar indexes up to logical decoding and reuse of free space

Covers the evening of 2026-11-19 and the night to 2026-11-20, after the
second entry.

### What was done

- **Phase 9.** An independent review of increments 9.1 and 9.2
  ([reviews/2026-11-19-kingres-shard-phase9-increments-1-2.md](reviews/2026-11-19-kingres-shard-phase9-increments-1-2.md),
  6bffb374d6) found one critical finding, P9A-01:
  rows written while `create_distributed_table` of a filled table ran
  could be lost. The fixes (3ff98847ec) race a writer against the call in
  each test. A call in `REPEATABLE READ` or `SERIALIZABLE` is refused,
  and a statement that touches a plain table and a distributed table
  together keeps a narrow window, which the design names. The same
  commit holds increment 9.3, the online rebuild engine with
  `create_distributed_table_concurrently`. Notes in
  [design/shard-phase9.md](design/shard-phase9.md).
- **Phase 2b.** The fixes for the review of step 5 (2eed174ef1): the
  findings P2D-01 to P2D-06 are fixed with tests, and the part of a
  statement that runs on the coordinator is no longer planned a second
  time before it runs.
- **Failover, increment 5** (8135c4b66e, REST contract 0.13.0): a
  failover of the server that holds the primary of the map group,
  answered by any member that cannot hear the leader, with a bootstrap
  journal that survives a restart of `kingresd`. The open items of the
  third review (F3-05, F3-08, F3-09, F3-10) have cluster tests now. The
  key of the certificate authority is not escrowed; a new leader without
  it raises `certificateAuthorityUnavailable`. Notes in
  [design/ha-phase5-part2.md](design/ha-phase5-part2.md).
- **Phase 7 in `kingresd`** (559c33b22c): the publication of a dropped
  database, kill tests, retention and clock warnings and evidence files.
  Corrections 187 to 189 and 204 to 206 of
  [design/ledger-phase7.md](design/ledger-phase7.md).
- **The verifier** (9e79ea83d3, 50b7cbf8cf): the fixes for its eighth
  review (V8-01 to V8-12), `logs.json`, and the reading of anchors
  (phase 7, steps 11, 13 and 15). Corrections 190 to 203.
- **Phase 12.** `kingres_columnar` 0.2: unique indexes, primary keys and
  index vacuum (C6c, d35f611dae), compaction of an indexed table (C6d,
  c33794b4c5), logical decoding of inserts (C7, ad9d1a334d) and reuse of
  free space in the file (C8, 3d83cc28c2). `kingres_timepart` 0.2
  converts partitions whose table has indexes (a3499aa2fc). Notes in
  [design/analytics-phase12-storage.md](design/analytics-phase12-storage.md).
- **Packaging** (5ef0cda036): the new versions of the extensions, and
  `kingres_columnar` in `shared_preload_libraries`.
- **The console** (3defb54c2f, a67d13f916): no test depends on the speed
  of the machine any more.
- **Two tests found by a complete integration run**, both faults of the
  test, not of the product: the freeze test lifted a freeze without the
  task that holds it, which is refused since F3-14; and the test of the
  settings validator set `statement_timeout` to 1 ms and then read it in
  a second statement, which that timeout cancelled under load.

### How it was verified

Run by the integrator on the tree that was committed, unless it says
otherwise.

- `kingres_shard` with the fixes of step 5: 28027 tests on 18.7, 28698 on
  19rc2, in tree 180 of 180. With phase 9 (9.1 to 9.3 and the fixes):
  29086 and 29811, in tree 187 of 187.
- `kingres_columnar` C7 and C8: in tree 65 of 65 twice, 11047 tests on
  19rc2.
- `kingresd` with increment 5: build for both systems, vet, format, 57
  unit packages on Windows, the contract check. A complete integration
  run of a14774e45f with the extensions of 8135c4b66e: 1496 top-level
  tests passed and 2 failed on 18, 1497 passed and 1 failed on 19; the
  failures are the two tests above. The rebalance tests passed three
  times on 19 and once on 18. The ledger cluster tests of phase 7 passed
  386 of 386 on 18 and on 19; one failure in a second run on 18 was fixed
  by 559c33b22c, and the complete run above passed that test.
- The verifier: 496 tests twice on Linux and a full byte sweep, the race
  detector, 27 packages on Windows. The panel: 1455 tests, the build.

### Decisions and deviations

- **C8 takes a lock** in `VACUUM` instead of walking the pages, which the
  design asked for; accepted, the reason is in the design notes.
- **Ledger correction 213**: `common-height` may be "unknown" in an epoch
  event, and the verifier must accept it and not rely on it.
- **Migrations** 0033 and 0034 belong to phase 7; failover increment 6
  starts at 0035.

### Still open

- **Pushes.** GitHub holds bfda9e8842 (18) and 91fcbffef0 (19). The head
  is pushed after a complete integration run of it on both majors with
  the two test fixes.
- **Reviews owed**: failover increments 4 and 5 with the F3 fixes, the
  publication and anchoring, steps 6 and later of phase 2b,
  `kingres_timepart` 0.2, columnar C5 to C8, the verifier's anchors, the
  console, packaging, increment 9.3 and the fixes of phase 9.
- **In two of twelve runs** the last step of two failover tests (resuming
  a paused failover after the heal) timed out; the cause is being looked
  for.
- `kingres_columnar` uses `RM_EXPERIMENTAL_ID` for its WAL; a custom id
  must be reserved before a release.

### Core patches

None.

---

## 2026-11-19 (second entry): anchoring, the fixes for the reviews of the publication and of the manual failover, closed queries in phase 2b, the first increments of phase 9, conversion of partitions and indexes on columnar tables, and the last console parts

Covers the morning and the afternoon of 2026-11-19, after the first entry.

### What was done

- **Phase 7 in `kingresd`.** The fixes for the review of the collector
  and the publication (68f1b4f91f, P8-01 to P8-10): every holder of the
  key files keeps a record of what was published outside the system
  database, and a database that is behind it signs nothing; a retirement
  is refused only for checkpoints the retiring key signed; a database
  counts as vanished after 30 minutes. Anchoring, steps 10, 12 and 14
  (02bd0e1168, 04b26dd13c, 4c5efc047d): the framework and the three
  kinds of target, a WORM object store, an RFC 3161 time-stamp authority
  and OpenTimestamps. Corrections 171 to 186 of
  [design/ledger-phase7.md](design/ledger-phase7.md).
- **Failover.** The fixes for the review of increment 3 (4574d26244,
  F3-01 to F3-15, REST contract 0.12.0): among them a failed failover no
  longer takes the lease from the primary it promoted, an old primary
  whose fencing cannot be proved refuses the failover, the verdict about
  lost writes counts only silence a majority shares, and a failover and
  a change of the databases exclude each other. The lease code no longer
  leaves requests in flight after a release. Notes in
  [design/ha-phase5-part2.md](design/ha-phase5-part2.md). The first piece
  of increment 5 (the journal of a bootstrap of the map group,
  bed0881cc3) is used by nothing yet.
- **Phase 2b, step 6** (4de9c4ce9d): a read whose result every shard
  group can compute by itself is run whole by each group, and the
  coordinator only unites, sorts and limits. The same commit holds the
  fixes for the two medium findings of the review of steps 3 and 4 (a
  key with a type modifier, and wide rows in the source of an
  `INSERT ... SELECT`).
- **Phase 9, increments 9.1 and 9.2** (8fa976c577, `kingres_shard` 0.7):
  a report of what prevents a change of a table, and
  `create_distributed_table` of a table that already holds rows. Notes
  in [design/shard-phase9.md](design/shard-phase9.md).
- **Phase 12.** `kingres_timepart` 0.2 (554a81bc4c): old partitions are
  converted to columnar storage and back, by hand or by a policy.
  `kingres_columnar`: a parallel scan (0ba0e045d3), and version 0.2 with
  row numbers reserved at insert time (75d62f13a1) and B-tree indexes
  that are not unique (65e050e5b1). Notes in a new section 21 of
  [design/analytics-phase12-storage.md](design/analytics-phase12-storage.md).
- **The console.** It follows contract 0.12.0; the failover console is
  complete (part 2: the automatic settings, the counter and the banner;
  part 3: switchover of a group and the restore of preferred primaries);
  the Ledger page has the tabs for anchoring and for client keys and the
  dialogs for bundles, the acknowledgement of a diverged chain and the
  audit of the publication.
- **Reviews**, one at a time: step 5 of phase 2b
  ([reviews/2026-11-19-kingres-shard-phase2b-step-5.md](reviews/2026-11-19-kingres-shard-phase2b-step-5.md):
  no shipped statement answered differently from a single node in 3600
  generated statements apart from `FROM ONLY`; two medium findings) and
  the verifier
  ([reviews/2026-11-19-kingres-verifier-publication-and-client-signatures.md](reviews/2026-11-19-kingres-verifier-publication-and-client-signatures.md):
  no input made it accept a wrong checkpoint, tile, proof or signature;
  four medium findings, the largest that a verified revocation of the
  signing key changes nothing in most commands). Its short check of the
  ledger extension found the three mutants of the last review of client
  signatures killed now.
- **A check of phases 12 and 13 against their designs** (not committed;
  its result is in the README): phase 13 is built apart from two
  warnings of the preview and five integration tests; of phase 12 only
  the three extensions exist.

### How it was verified

Run by the integrator on the tree that was committed, unless it says
otherwise.

- `kingres_shard` 0.7 with step 6: PostgreSQL 18.7, 18 regression tests
  and 133 TAP files with 27732 tests; 19rc2, 28461 tests; in tree on 18
  with assertions 173 of 176 on a loaded machine, and the three (two
  timing sweeps and the bulk memory test that ran into its time limit)
  passed alone. Phase 9 alone before it: 20672 and 21377 tests; the one
  in-tree failure was a test that lost its include paths when it started
  itself again under a memory limit, fixed.
- `kingres_columnar`: in tree 47 of 47 twice (parallel scan), 53 of 53
  twice (reservation), 55 of 55 (indexes; in a first run one assertion
  of the backup test of an earlier round failed and the test passed
  three times alone); on 19rc2 43, 50 and 52 TAP files with 10373, 10716
  and 10740 tests. `kingres_timepart` 0.2: in tree 12 of 12 twice, on
  19rc2 16 regression tests and 8 TAP files with 491 tests, again after
  `kingres_columnar` 0.2.
- `kingresd`: build for both systems, vet, format, 54 to 56 unit packages
  on Windows and the contract check at each commit. On clusters: the two
  failover tests that failed in the complete run passed five times on 18
  and three times on 19; the rebalance takeover test three times on 19;
  the cluster tests that use distributed tables with the extension of
  step 5 passed 26 of 26 on 18 and 25 of 26 on 19 (see "Still open").
- The panel: 87 to 90 test files with 1381 to 1455 tests at each commit,
  the browser tests 3 files with 46, type check, lint, the contract and
  brand checks and the build. On a heavily loaded machine one to four
  tests that wait for the page failed, a different one in each run; a
  run on a quieter machine passed all.

### Decisions and deviations

- **Version numbers.** `kingres_shard` 0.7 is phase 9; `kingres_timepart`
  0.2 and `kingres_columnar` 0.2 change SQL objects and have upgrade
  scripts. No release exists, so nothing is renumbered.
- **The console follows the server where the server and the design
  differ**, for anchoring in particular (member names and limits of the
  targets, a credentials file, removal of stored credentials).
- **Indexes on columnar tables in four parts**, decided where the design
  is silent: a stripe's worth of row numbers is reserved per buffer; a
  subtransaction that commits writes a reserving buffer as a stripe of
  its own; compaction of an indexed table will rebuild its indexes; no
  bitmap or index-only scans.
- **A cluster batch holds the machine-wide lock of the integration runs**
  for hours. Writers now queue one batch per round and wait for it.

### Incidents on the build machine

- **A weekly usage limit stopped every writer and reviewer twice** in the
  afternoon, in the middle of their work; their trees kept what they had
  written, and they were resumed from where they stopped. Until the limit
  resets the integrator verifies and commits alone.
- **A restart of the session lost the record of four background runs**
  while their processes went on; they were found alive and followed by
  new watchers.
- **One writer started a cluster test without the lock of the
  integration runs**; the lock is now named in every brief.

### Still open

- **Pushes.** Nothing of this day is on GitHub yet. The cluster test
  `TestRebalance` failed once on 19 with the extension of step 5 ("a move
  that fails is retried": the run after it paused because
  `check_cluster()` reported `catalog_differs` on two nodes); it is being
  repeated alone. The first batch (up to bfda9e8842) is pushed when that
  is explained; the rest after a complete integration run of the head on
  both majors.
- **Reviews owed**: failover increment 4 with the F3 fixes, the fixes for
  the publication and anchoring, phase 9 (its review was stopped by the
  usage limit), step 6 of phase 2b, `kingres_timepart` 0.2, the parallel
  scan and the indexes of `kingres_columnar`, the console parts.
- **Open findings**: P2D-01 to P2D-06 of step 5 (a `reg*` parameter or
  array shipped as a name; `FROM ONLY`; `debug_discard_caches`;
  `place=0`); V8-01 to V8-12 of the verifier; P2C-05, P2C-06 and the
  replanning hazard of the local part of a write in phase 2b; the reset
  of identity sequence options by `create_distributed_table`.
- **Being built when the writers stopped**: failover increment 5 (the map
  group), the remaining items of phase 7 (the measurement of blocks of a
  quiet database, the fixture of a whole publication, kills inside a
  round, the retention warnings), the rebuild engine of phase 9 (9.3),
  unique indexes and index vacuum of `kingres_columnar`, the fixes of the
  verifier and its reading of anchors, step 7 of phase 2b.
- **Phase 12 outside the extensions** (contract, control plane, console,
  the parts in `kingres_shard`) and phases 10 and 11 are not started.

### Core patches

None.

---

## 2026-11-19 (first entry): the row router and the colocated INSERT ... SELECT of phase 2b, the old primary back as a standby, the keys, the collector and the publication of the ledger, and the verifier that reads them

Covers the evening of 2026-11-18 and the night and early morning of 2026-11-19, up to the step 5 of phase 2b and its review of steps 3 and 4 (bfda9e8842).

### What was done

- **Phase 2b, steps 3 and 4 of thirteen** (f6484e3dfa; notes in
  [design/shard-phase2b.md](design/shard-phase2b.md) under "Steps 3 and
  4"): a multi-row `INSERT`, an `INSERT ... SELECT` and `COPY FROM` that
  load rows for several shard groups are routed row by row, in bounded
  memory, in one distributed transaction. The same commit holds the
  fixes for the review of steps 1 and 2 (P2B-01 to P2B-09) and for the
  sixth review of the call sequences (S14-01 to S14-08, report
  [reviews/2026-11-18-kingres-shard-0.5-sixth-sequences.md](reviews/2026-11-18-kingres-shard-0.5-sixth-sequences.md)).
- **`kingres_shard` 0.6** (be308c7e12): a version with no change of a
  table, a view or a signature, made before the remaining steps of the
  phase (see "Decisions").
- **Phase 2b, step 5** (7aa46ebb9d; notes under "Step 5"): the analysis
  module that the read pushdown builds on (`src/pushdown.c`), and its
  first user: an `INSERT ... SELECT` whose target and sources are placed
  together is run whole by each shard group.
- **Failover in `kingresd`, increment 4 of ten** (cf390ffee8; notes in
  [design/ha-phase5-part2.md](design/ha-phase5-part2.md) under
  "Increment 4"): the server that held the old primary becomes a standby
  of the new one, by `pg_rewind` or by a base backup, and only when its
  directory is proved to be a former primary. The rest of the second
  review (F2-06, F2-08, F2-09, F2-16, a test for F2-02) is in the same
  commit, and a defect of increment 3 (the second group of a failover of
  two groups ended "superseded").
- **The failover console, part 1 of 3** (6b2f49fc4c): the dialog with
  the plan of a dry run, the progress card and the report.
- **Phase 7 in `kingresd`, steps 3 and 4** (3e36e43d9b; contract 0.11.0;
  corrections 121 to 136 of
  [design/ledger-phase7.md](design/ledger-phase7.md)): the 28 operations
  of the phase in the contract, a scheduler of the leader, and the
  signing keys of the cluster with their events and the trust file. The
  fixes for their review (77a72ce498, P7-01 to P7-16).
- **Phase 7 in `kingresd`, steps 5 and 6** (ca29bf1521; corrections 137
  to 149): the leader reads the ledger of every database from every
  shard group, keeps a transparency log per chain and an aggregate log
  per database, signs their checkpoints and serves the publication on a
  listener of its own.
- **The verifier, steps 7 and 18 of phase 7** (cebc50a5a7; corrections
  160 to 170): it reads a publication from a directory, an archive or an
  address, checks an aggregate, a bundle and a client signature, follows
  the key events from a pinned trust file, and has a monitor that never
  moves backwards.
- **The panel follows contract 0.11.0, and the Ledger console, part A**
  (d7d7eb0384): overview, digests, signing keys and publication.
- **`kingres_ledger` 0.3, two fix rounds for client signatures**
  (30afc40830, L21-01 to L21-08, corrections 150 to 158; 99d50b6d65,
  L22-01 to L22-07). The critical one: a transaction that was open when
  a table was switched to "signature required" could commit entries
  without an attestation afterwards.
- **`kingres_columnar` 0.1, the fixes for the twelfth review**
  (d8b2765084, C12-01 to C12-05), and the track of its reviews closed by
  a closing check
  ([reviews/2026-11-18-kingres-columnar-0.1-closing-check.md](reviews/2026-11-18-kingres-columnar-0.1-closing-check.md)).
- **Nine independent reviews**, one at a time, each on an export of a
  commit: the sixth of the call sequences (S14), failover increment 3
  (F3), the seventh of the ledger guard with the first of client
  signatures (L21), the second of client signatures (L22), the twelfth
  of the columnar extension (C12) and its closing check, the signing
  keys (P7), the collector and the publication (P8), and steps 3 and 4
  of phase 2b (P2C). The reports are under [reviews/](reviews/) with the
  dates 2026-11-18 and 2026-11-19.

### How it was verified

Run by the integrator on the tree that was committed, unless it says
otherwise.

- `kingres_shard` at f6484e3dfa: out of tree on 18.7, 14 regression
  tests and 119 TAP files with 14516 tests; on 19rc2, 15175 tests; in
  tree on 18 with assertions 162 of 162; no warnings. At be308c7e12 the
  same suite again with the same totals. At 7aa46ebb9d: 16 regression
  tests and 123 files with 20162 tests on 18.7, 20792 on 19rc2; in tree
  163 of 165, the two failures being sweeps of `promote_node` that
  passed two of two alone on a quiet machine.
- `kingres_ledger` at 99d50b6d65: in tree on 18.7 with assertions 42 of
  42 twice; out of tree on 19rc2 19 regression tests and 38 TAP files
  with 2571 tests.
- `kingres_columnar` at d8b2765084: in tree 46 of 46 twice; on 19rc2 11
  regression tests and 42 TAP files with 10265 tests.
- The verifier at cebc50a5a7: 409 tests in 22 packages on Windows, 444
  in 23 packages on Linux twice. With the race detector no data race;
  two older tests that bound the resident memory fail under it and
  should skip there.
- The panel at d7d7eb0384: 81 test files with 1224 tests, the browser
  tests 3 files with 43, type check, lint, the contract and brand checks
  and the build.
- `kingresd`: build for both systems, vet, format and the unit packages
  on Windows at every commit. Gates on real clusters after each change
  of an extension (the cluster tests that use distributed tables and
  standbys, and those that use the ledger) on both majors. A complete
  integration run of the tree with increment 4, steps 5 and 6 of phase
  7 and `kingres_shard` 0.6, about 85 minutes per major: on 18.7, 1406
  passed, 24 skipped and 2 failed
  (`TestASecondFailoverWhileOneIsPaused`, where the leader lost the
  lease of its own group in the middle of the test, and
  `TestOldPrimaryRunningUnfenced`, where the reading of the topology
  answered 503 at the formation of the cluster, a defect that the notes
  of increment 4 name); on 19rc2, 1407 passed, 24 skipped and 1 failed
  (`TestRebalanceTakeoverAtTheCatchUp`, a writer of the test lost its
  connection). Each of the three passed alone afterwards (the two failover
  tests five times on 18 and three times on 19, the rebalance test three
  times on 19); the two failover tests read a state once while the leader
  changed, and wait for it now (4574d26244).

### Decisions and deviations

- **One commit per round of a writer.** Steps 3 and 4 of phase 2b and
  the fixes for two reviews were written together and could not be
  separated without rewriting; they are one commit that says so.
- **Version 0.6 before steps 5 to 12 of phase 2b**, against section 14
  of its design: the table lifecycle (phase 9) was started beside the
  remaining steps and builds its catalog on 0.6. Phase 9 is therefore
  0.7, phase 10 is 0.8 and phase 11 is 0.9, each one higher than its own
  design says.
- **Two writers in one directory.** When a branch of a worktree had to
  enter a directory in which another writer had uncommitted work, the
  index was set to the versions of the branch, the files that did not
  overlap were written to the working tree, and each overlapping file
  was merged by `git merge-file` and looked at by hand; the commit was
  compared with the branch afterwards.
- **A database that vanished without a call of the control plane**
  retires its chains after no shard group has had it for two rounds of
  the collector, with a signed event; the alert for it is not built.
- **A guard review of the ledger is not repeated**: the seventh held.
  The columnar track, the anchoring libraries and the key and log
  libraries are closed in the same way; a later change of them is
  reviewed as that change.
- **The order of the unpublished commits was changed once** before they
  were pushed (cherry-picked onto the published head and compared with
  the old tip: no difference). Nothing that was published was rewritten.

### Incidents on the build machine

- **The system disk fell to 13 GB free.** The build cache of Go on
  Windows, the page cache of WSL and finished work areas were removed;
  about 20 GB of temporary files of other projects were left alone.
- **Writers that end their turn while a run of theirs still goes**: the
  first author of steps 3 and 4 handed back unfinished three times and
  was replaced; the integrator reads the logs of such runs itself and
  repeats the suite on the tree it commits.
- **A verifier author downloaded a Go toolchain** from go.dev into its
  own work area without checking its checksum. Nothing of it is in the
  repository; the checks of the integrator use the toolchain that was
  there before.
- **The first test of a new extension version fails on 18**: the test
  of a cluster of two majors uses the extension installed for the other
  major, so both installations must hold the new version first.

### Still open at the end of this part (see the next entry for what changed)

- **Failover**: the findings of the review of increment 3 (F3-01 to
  F3-15, four of them high, found by reading) are being fixed with
  increment 5; increments 6 to 9; the review of increment 4.
- **Phase 7**: the review of the collector and the publication found
  one critical defect (P8-01: a restore of the system database forks the
  aggregate log and the events) and two high ones; they are fixed before
  the anchoring (step 10) that is being written. The publication is not
  to be relied on until then.
- **Phase 2b**: P2C-01 (a key with a type modifier that changes the
  value makes a multi-row insert fail) and P2C-02 (the source cursor is
  counted in rows, not in bytes), the tests the review asks for, and a
  defect of the row router with `GENERATED ALWAYS AS IDENTITY`, in a
  round before step 6; steps 6 to 12; the review of step 5 is running.
- **Phase 9**: increments 9.1 and 9.2 (version 0.7) are written on a
  branch and are being verified.
- **The contract**: `failover_needs_decision` has no schema member for
  its plan; the text of `notScheduled`; no operation for a backup alone;
  a task has no result for a bundle.
- **The verifier**: the table of Bitcoin headers is older than a year
  and must be extended before a release.

### Core patches

None.

---

## 2026-11-18 (third entry): a failover started by hand, client signatures in the ledger, and the fixes for three reviews

Covers the afternoon of 2026-11-18.

### What was done

- **Failover in `kingresd`, increment 3 of ten, with most fixes for the
  second review** (2de4b63734; contract 0.10.0; notes in
  [design/ha-phase5-part2.md](design/ha-phase5-part2.md) under
  "Increment 3 and the fixes for the second review"). An operator starts
  a failover of a server by hand; a dry run answers the plan; a plan that
  may lose writes needs an explicit decision; a leader that ends at a
  step boundary is taken over. The first failovers on a real cluster
  showed eight defects of the product, the largest of them that each
  database with the extension has a catalog of nodes of its own, so a
  standby must be registered, and `promote_node` run, in every one.
- **The panel follows contract 0.10.0** (fec869816a): words, the mock of
  the new operations, the narrowed answers. The failover pages are a
  later increment and are being built.
- **`kingres_ledger` 0.3, part B: client signatures** (5e53dcd8a0, step 8
  of phase 7; corrections 111 to 120 of
  [design/ledger-phase7.md](design/ledger-phase7.md), 6391678a95). A
  client registers a public key and signs what it writes; a table can
  require it, and then a transaction that leaves an entry without an
  attestation cannot commit or be prepared.
- **`kingres_shard` 0.5, the fixes for the fifth review of the call
  sequences** (4b5a771932, S13-01 to S13-06): `RELEASE SAVEPOINT` and
  `ROLLBACK TO` are bounded for every session, and a control call can be
  bounded as a whole (`kingres_shard.control_call_timeout`, `KS015`).
- **`kingres_columnar` 0.1, the fixes for the eleventh review**
  (3f3caad6de, C11-01 to C11-09; report
  [reviews/2026-11-18-kingres-columnar-0.1-tenth-fixes.md](reviews/2026-11-18-kingres-columnar-0.1-tenth-fixes.md)):
  a columnar relation is recognised by the handler of its access method
  everywhere, the check gives no false alarm while tables are dropped or
  rewritten and keeps no lock per table.
- **The review of steps 1 and 2 of phase 2b**
  ([reviews/2026-11-18-kingres-shard-phase2b-steps-1-2.md](reviews/2026-11-18-kingres-shard-phase2b-steps-1-2.md)):
  about 22 000 generated statements compared with a single node on both
  majors, no wrong answer; one high finding (P2B-01, the mark of the
  local part can be read by SQL code the statement runs), three medium.
- **Steps 3 and 4 of phase 2b** (the row router for `INSERT` and
  `COPY FROM`) are written and not finished: see "Still open".

### How it was verified

Run by the integrator on the tree that was committed, unless it says
otherwise.

- `kingresd` at 2de4b63734: build for both systems, vet, format, 49
  packages on Windows, the contract check; a complete integration run on
  18.7 and on 19rc2 from the tree before the commit: 1255 tests passed
  and 24 were skipped on each, and one failed
  (`TestObjectsBeforeAndAfterExistOnAllServers`, which counted every node
  of a database's catalog; it counts the primaries now). That test and
  the failover tests again on both majors against `kingres_shard` with
  the S13 fixes: 9 of 9.
- The panel at fec869816a: type check, lint, the contract and brand
  checks, the build, 74 test files with 1023 tests, the browser tests 3
  files with 43.
- `kingres_ledger` at 5e53dcd8a0: in tree on 18.7 with assertions 38 of
  38 twice; out of tree on 19rc2 19 regression tests and 34 TAP files
  with 1929 tests; no warning. The tests of `kingresd` that use the
  ledger against it: all passing on both majors; one test that has
  nothing to do with the ledger failed once on 19 (it read the state of
  an instance in the moment between the end of the postmaster and the
  record of the stop; corrected in fc0ff6ff2f, three of three on 19
  afterwards).
- `kingres_shard` at 4b5a771932: on 18.7 13 regression tests and 111 TAP
  files with 12464 tests; on 19rc2 13 and 111 files with 13123 tests; in
  tree on 18 with assertions 146 of 146; the cluster tests of `kingresd`
  that use distributed tables and standbys, 19 of 19 on each major.
- `kingres_columnar` at 3f3caad6de: in tree on 18.7 with assertions 45 of
  45 twice; out of tree on 19rc2 11 regression tests and 41 TAP files
  with 10216 tests.
- Shown by the authors only: the mutants of each round, the measured
  cost of an attestation, a production build of 18 for the columnar
  suite.

### Decisions and deviations

- **The increment and the review fixes of failover are one commit.**
  The author could not separate them by file, and a commit made of
  hunks would have been a tree nobody built or tested.
- **`kingresd` counts the ledger tables of users only** (102c6e81cf):
  the extension registers its own three tables in the same registry.
- **The seventh review of the guard of the ledger is merged with the
  review of part B**, since both rounds changed the same files.
- **Two writers in the control plane**: failover continues in the main
  tree; the first steps of phase 7 in `kingresd` are written in a
  worktree on a private branch that merges the product branch before it
  is delivered. Migrations of phase 7 start at 0030.
- **Contract numbers**: the design of phase 7 names its revision 0.9.0
  as a placeholder; it is 0.11.0.

### Still open

- **Failover**: F2-06, F2-08, F2-16, the design statements of F2-14,
  the other direction of F2-09, a test for F2-02; increments 4 to 9
  (increment 4 is being written); the review of increment 3 is owed.
- **Phase 2b, steps 3 and 4**: the code and its own tests exist; the
  full suites, the assertion build and the mutants are owed, the older
  tests that assert a refusal the steps remove are being changed, and
  the fixes for P2B-01 to P2B-09 go into the same round.
- **Reviews owed**, one at a time: the fixes for S13 (running), failover
  increment 3 with the F2 fixes, the guard of the ledger with part B,
  the fixes for C11.
- **`kingres_ledger`**: `ledger_verify` does not verify signatures, and
  the standalone verifier does not check attestations yet (step 18 of
  phase 7); `kingresd` reports the new check names of `ledger_verify`
  as "other".
- **The contract**: the answer `failover_needs_decision` is documented
  as carrying the plan, and the problem schema has no member for it.

### Core patches

None.

---

## 2026-11-18 (second entry): step 2 of phase 2b, the fixes for the first failover increments, the libraries of phase 7 closed, and fix rounds with their reviews

Covers the work from the early morning to noon of 2026-11-18.

### What was done

- **Phase 2b, step 2 of thirteen** (f7872a2065, notes in
  [design/shard-phase2b.md](design/shard-phase2b.md) under "Step 2" and
  "What step 3 will find changed", 1fd06a4e06): the check that the tables
  of a query are placed together, pruning of the shard groups from the
  conditions on the distribution column, a nonce for the part that runs
  on the coordinator itself, and reads that join a local table, which
  are answered by the scan that moves rows. No query is pushed down as a
  whole yet; that begins with step 3.
- **`kingres_shard` 0.5, the fixes for the fourth review of the call
  sequences** (d98a42b7ee, S12-01 to S12-06): a remote transaction no
  longer sets `lock_timeout`, each statement of a control call has its
  own bound, and a refusal of a node is classed by the node's SQLSTATE.
  The fifth review
  ([reviews/2026-11-18-kingres-shard-0.5-fifth-sequences.md](reviews/2026-11-18-kingres-shard-0.5-fifth-sequences.md))
  found one high defect in what is published (S13-01, see "Still open").
- **Failover in `kingresd`**: the fixes for FO-01 to FO-16 and for the
  rest of ST-05 to ST-10 (3f48af6945, f47960a975, design notes
  57533db902). Their review
  ([reviews/2026-11-18-kingresd-failover-fixes.md](reviews/2026-11-18-kingresd-failover-fixes.md))
  has seventeen findings, one of them high (F2-01: the list of a frozen
  group changes when a standby is added). They are being fixed together
  with increment 3 (a failover started by hand).
- **`kingres_ledger` 0.3, the guard**, two more fix rounds and two
  reviews (04f4963266 for L17, 1c412b7317 for L19; reviews
  [fifth](reviews/2026-11-18-kingres-ledger-0.3-guard-fifth-review.md)
  and
  [sixth](reviews/2026-11-18-kingres-ledger-0.3-guard-sixth-review.md)).
  The sixth review showed that the timer which bounds a hold could still
  be lost inside an aborted subtransaction. The rule was made simpler
  instead of the tracking more exact (see "Decisions").
- **The verifier**, two fix rounds (aa9c9b7b51 for L16, 62ec7dd648 for
  L18) and the review between them
  ([reviews/2026-11-18-kingres-verifier-seventh-fixes.md](reviews/2026-11-18-kingres-verifier-seventh-fixes.md),
  the eighth of this component), whose reviewer advised to stop
  reviewing it. The track is closed.
- **`kingres_columnar` 0.1**: the band in the wraparound test and a log
  line for autovacuum on a columnar table (017df7e72e), the fixes for
  the tenth review (8f06094292, C10-01 to C10-08), the review of the
  ninth round
  ([reviews/2026-11-18-kingres-columnar-0.1-ninth-fixes.md](reviews/2026-11-18-kingres-columnar-0.1-ninth-fixes.md)).
  The eleventh review is running.
- **Phase 7, the anchoring libraries**: fix rounds three to five
  (4eaa526cc2, 7730bbbf56, 11c06466ba) with the
  [third](reviews/2026-11-18-kingresd-anchoring-libraries-third.md),
  [fourth](reviews/2026-11-18-kingresd-anchoring-libraries-fourth.md) and
  [fifth](reviews/2026-11-18-kingresd-anchoring-libraries-fifth.md)
  reviews. The track is closed; what a caller must do is in the section
  "Duties of the caller" of the package documentation.
- **Phase 7, the libraries for keys and logs**: three reviews
  ([first](reviews/2026-11-18-kingresd-key-and-log-libraries.md),
  [second](reviews/2026-11-18-kingresd-key-and-log-libraries-second.md),
  [third](reviews/2026-11-18-kingresd-key-and-log-libraries-third.md))
  and three fix rounds (67026e0b53, c5c07c43e6, 316bf4ea7e). The third
  review judged them fit to be wired into the daemon; the last round is
  small and is covered by the review of the wiring.
- **Designs**: corrections 85 to 110 of
  [design/ledger-phase7.md](design/ledger-phase7.md).
- **The integration run** allows the skips of the library tests that
  need third-party vectors or run long (5771c52700, b9d4347549).

### How it was verified

Run by the integrator on the tree that was committed, unless it says
otherwise.

- `kingres_shard` at f7872a2065: on 18.7 13 regression tests and 106 TAP
  files with 10064 tests; on 19rc2 106 TAP files with 10723 tests; in
  the tree of `kingres-19` 190 passed, none failed, one skipped. The
  cluster tests of `kingresd` that use distributed tables, routing,
  rebalancing and standbys, from an export of the committed control
  plane against the new extension: 19 of 19 on each major.
- `kingresd` at 3f48af6945 and f47960a975: complete integration runs,
  938 passed and one failed on 19rc2; 1112 passed and one failed on 18.7
  (`TestClientEntryPoint`, ten of ten alone afterwards, see "Still
  open").
- `kingres_ledger` at 1c412b7317: in tree on 18.7 34 of 34 twice; out of
  tree on 19rc2 18 regression tests and 30 TAP files with 1521 tests; no
  warning on either major. The tests of `kingresd` that use the ledger
  are being run against it; it is not published before they pass.
- The verifier at 62ec7dd648: on Linux and on Windows (328 passing and
  22 skipped, twice).
- `kingres_columnar` at 8f06094292: in tree 42 of 42 twice after a race
  in a test helper was removed (the session now reports its own process
  id).
- The anchoring libraries at 11c06466ba: on Linux 294 passing and 17
  skipped, four runs (two with the race detector); on Windows 292 and
  19.
- The libraries for keys and logs at 316bf4ea7e, with a built verifier
  required: on Linux 162 passing and 3 skipped, twice plain and twice
  with the race detector, the long run 165, without a verifier 155 and
  10 skipped; on Windows 151 and 14 skipped in four runs of five, the
  fifth failing on the removal of a test's temporary directory.
- Shown by the authors and the reviewers only: that each new test fails
  without its fix, the mutants, the fuzz runs.

### Decisions and deviations

- **A role that is not a superuser calls a lock-taking function of the
  ledger only as a statement of its own** (correction 108). The timer
  that bounds the hold is then never armed inside a subtransaction, the
  tracking per subtransaction is removed, and while the timer is armed
  the transaction control statements are refused. A procedure or a
  `DO` block of such a role that calls these functions gets `25001`.
- **The time `ledger_enable` adds to the limit is bounded**
  (`kingres_ledger.enable_credit_limit`, 30 minutes; correction 109).
- **A note is signed again only when a key that counts vouches for it**
  (correction 107), and every time a key set records is bounded against
  its own latest record, not against a clock the package does not have.
- **No further library review** for the verifier, the anchoring
  libraries and the libraries for keys and logs. The next review of
  them is the review of their wiring into `kingresd`.

### Incidents on the build machine

- The system drive fell to 10 GB free: a recursive listing left running
  in the background had grown the page file to 47 GB. Stopped; the page
  file shrank by itself.
- WSL ran out of memory at 06:50: a reviewer's mutant removed an
  allocation bound and a test then allocated 31 GB. The WSL service
  failed afterwards and was restarted; every run of that window was
  repeated. Since then every brief bounds a test or probe to 2 GB.
- The system drive fell to 13 GB free again at noon (the page file and
  the virtual disk of WSL); the Go build cache on Windows, the page
  cache of WSL and the work areas of finished rounds were cleared.
- A load average above about 16 makes the timing-sensitive suites fail
  (connections between nodes that time out, a server that does not
  start in its time); such runs were repeated alone before they were
  judged.

### Still open

- **`kingres_shard`, S13-01 (high, published)**: `RELEASE SAVEPOINT`
  after a control call inside a subtransaction has no bound. The fix,
  with S13-02 to S13-06, is being written; step 3 of phase 2b (the batch
  sender and the row router) starts when it is integrated, since both
  change the connection code.
- **Failover, F2-01 (high, published) and F2-02 to F2-17**, then
  increments 3 to 9.
- **`kingres_ledger`**: the fix for L19 is committed and not published
  until the tests of `kingresd` pass against it; its review (the
  seventh of the guard) is owed.
- **Reviews owed**, one at a time: the eleventh of `kingres_columnar`
  (running), the fix for L19, steps 1 and 2 of phase 2b, the fix for
  S13, the fixes for F2 with increment 3.
- **Two tests that depend on timing**: `TestClientEntryPoint` reads the
  entry point right after the cluster is formed (failed once in a
  complete run); the liveness subtest "a step of the wall clock changes
  nothing" failed in a part of the repeats on 19rc2 before and after the
  failover fixes (the reading of the review: the primary that the
  watchdog fenced during the previous subtest is still restarting).
- **The table of Bitcoin block headers** in the OpenTimestamps package
  is older than a year; it must be extended before a release, which
  needs a source the project owner chooses.
- **`kingresd` and the owner of a database**: unchanged from the first
  entry of this day.
- **Phase 7 in `kingresd`** (steps 3 to 25) has not begun; it starts
  from the duties of the caller that the three libraries state.

### Core patches

None.

---

## 2026-11-18: failover begun in kingresd, the libraries of phase 7 for keys, logs and anchoring, and further fix rounds with their reviews

Covers the work from the afternoon of 2026-11-17 to the early morning of
2026-11-18.

### What was done

- **Failover in `kingresd`, increments 0 to 2 of ten** (191bba73ee,
  design [design/ha-phase5-part2.md](design/ha-phase5-part2.md)): a
  standby is registered in the catalog of `kingres_shard`; an instance has
  a role file and leases, markers and watchdogs work per group, so that a
  server can hold the primary of more than one group; a lease can be
  transferred and released, and there are read calls for the position of
  a standby and the state of a failover. Failover itself is not in it.
  The same commit fixes ST-01 to ST-04 of the review of rebalancing, and
  f895d8fb26 fixes ST-05 to ST-10 (a move is matched with its record by a
  mark and not by two clocks; a live move is waited for; a server that
  keeps failing a configuration is retried with a growing pause). The
  review of the three increments
  ([reviews/2026-11-17-kingresd-failover-increments-0-2.md](reviews/2026-11-17-kingresd-failover-increments-0-2.md),
  641098f736) is incomplete by its own statement and has five high
  findings (FO-01 to FO-16); their fixes are written and are being
  verified (see "Still open").
- **`kingres_ledger` 0.3, part A**, three fix rounds and two reviews
  (7803a5435f, 213fad5c2c, 62b3916dee; reviews
  [third](reviews/2026-11-17-kingres-ledger-0.3-guard-third-review.md),
  28af1550c7, and
  [fourth](reviews/2026-11-17-kingres-ledger-0.3-guard-fourth-review.md),
  78bf9b3bfd). A second invariant beside the one for writes: a role that
  is not a superuser cannot stop the builder or the capture for longer
  than one statement of its own and cannot remove the ledger by a
  statement in its database. On the way: the refusal of a lock follows
  the relation that is locked; nothing of the ledger is dropped through a
  dependency; no writer takes an identifier from a sequence; a role that
  holds a lock of the extension for longer than
  `kingres_ledger.lock_hold_limit` has its session ended, cannot prepare
  the transaction, and the limit follows savepoints. The fourth review
  found that the README claimed too much about `DROP DATABASE`; the claim
  is withdrawn and `ledger_status` lists the roles that can drop the
  database.
- **The verifier**, two fix rounds and three reviews (ce8993da0e,
  dc0d74fcbb; reviews
  [L12](reviews/2026-11-17-kingres-verifier-fourth-fixes.md), 6b1d874503,
  [L14](reviews/2026-11-17-kingres-verifier-fifth-fixes.md), 9a2ef8200a,
  [L16](reviews/2026-11-18-kingres-verifier-sixth-fixes.md), 28cab0dae3).
  The identity of a checkpoint is its signed text and never its signature
  lines, which only accumulate; a held checkpoint stays the anchor when
  its keys were revoked, is named weak, and does not accuse the log when
  it disagrees with it; the lock file is never replaced and a state is
  never written over bytes the run did not read; limits on a proof
  element. The last review found no status 0 for something that was not
  verified and one medium finding about which conflict a weak state
  keeps.
- **`kingres_columnar` 0.1**, two fix rounds and one review (f64e8d7aa6,
  f8c196d043; review
  [reviews/2026-11-17-kingres-columnar-0.1-eighth-fixes.md](reviews/2026-11-17-kingres-columnar-0.1-eighth-fixes.md),
  1fed5e7370). Every entry point of the access method has a recorded
  decision for the moments at which the server forbids an error; a
  refusal at a moment at which the transaction is still alive makes the
  transaction fail before it can commit; a critical section entered
  after the extension's own pre-commit is treated as decided.
- **Phase 7, libraries** that `kingresd` does not call yet: keys and
  logs (f16fc5feae: an encrypted key file with rotation, the Merkle tree
  in tiles, checkpoints as signed notes, proofs, the publication
  directory); anchoring after two reviews and two fix rounds
  (5a58604194, 9740c2546a; reviews
  [first](reviews/2026-11-17-kingresd-anchoring-libraries.md),
  fd88571cbc, and
  [second](reviews/2026-11-18-kingresd-anchoring-libraries-second.md),
  a895b7383a). The first review found that a record was called anchored
  on the word of an OpenTimestamps calendar; a calendar's attestation is
  now a claim until a block header from a source the operator configures
  confirms it.
- **Designs**: corrections 49 to 84 of
  [design/ledger-phase7.md](design/ledger-phase7.md) (cf68ae0d42,
  33bd984e4a, 92834ccce9, ef39325c55, fda76c8296) and the section "Found
  while building" of the failover design.

### How it was verified

Run by the integrator on the tree that was committed, unless it says
otherwise.

- `kingresd`: at 191bba73ee, build for both systems, vet, format, unit
  tests, the contract check, and a complete integration run on each
  major: 843 passed on 18.7; on 19rc2 842 passed and one failed
  (`TestInstanceMappingAfterAHandFailover`, see "Still open"). At
  f895d8fb26: 919 passed, none failed on 18.7; 917 passed and two failed
  on 19rc2 (the same test and one that passed five of five repeats).
- `kingres_ledger`: at 213fad5c2c in tree on 18 twice, 24 of 24, and 997
  tests on 19rc2; at 62b3916dee in tree 27 of 27, then 26 of 27 (a test
  that waited for a subscription in the wrong database, corrected by the
  integrator), then eight runs of that test beside a burner and 27 of 27
  again; on 19rc2 18 regression tests and 23 TAP files with 1138 tests.
  The tests of `kingresd` that use the ledger were run against each
  version of the extension from an export of the committed control
  plane.
- The verifier at dc0d74fcbb: on Linux three runs of 336 top-level
  tests, none failing; on Windows four of seven runs clean (297 passing,
  22 skipped) and three with one test failing on the removal of its
  temporary directory ("The directory is not empty"). The review of that
  round reproduced the message with a program that uses no code of the
  verifier and holds no handle, and found no handle left open by the
  verifier; which outside program holds the entry is not established.
- `kingres_columnar`: at f64e8d7aa6 in tree on 18 twice, 37 of 37, and
  33 TAP files with 7352 tests on 19rc2; at f8c196d043 in tree 37 of 38 and 38 of
  38, and on 19rc2 34 TAP files with 9783 tests, all passing. The one
  failure is `t/013_xid_wraparound` (see "Still open").
- The libraries: keys and logs on Linux with the long tests and a built
  verifier 87 of 87, twice plain and twice with the race detector by
  default (85 and 2 skipped), on Windows 83 and 4 skipped; anchoring at
  9740c2546a on Linux 214 passing and 17 skipped (the third-party
  vectors, which are not in the repository), twice plain and twice with
  the race detector, each test binary also in an empty directory, on
  Windows 212 and 19 skipped.
- `kingres-19` after each synchronisation: built in the tree without a
  warning and the suites of the extensions run. At e62a6cd4e0, 170
  passed, one was skipped and three tests of `kingres_shard` failed on
  connections between nodes that timed out after 5 seconds while the
  load average was above 16; alone they passed eight of nine runs, the
  ninth at a load of 28. The code of `kingres_shard` is the one verified
  on 18.
- Shown by the authors and the reviewers only: that each new test fails
  without its fix, the mutants, the fuzz runs, the measurements.

### Decisions and deviations

- **A held checkpoint outlives its keys** (corrections 55 and 69): a
  revocation says that a key must not be trusted for new notes, not that
  a checkpoint the auditor verified is unknown to them.
- **An OpenTimestamps attestation is a claim** (corrections 60 and 82),
  and a claim never counts as an anchor.
- **Identifiers of the ledger do not come from sequences** (corrections
  66 and 77): a member of `pg_write_all_data` can move a sequence and no
  hook sees it.
- **`DROP DATABASE` cannot be prevented from inside the database**
  (corrections 68 and 74). What protects a deployment is the ownership
  of the database, which the control plane has to ensure; it does not
  yet (see "Still open").
- **Third-party test vectors are not in the repository** (the AWS
  signature suite and the OpenTimestamps examples): the tests that need
  them skip, and the same properties are tested with data of our own.
  Whether they may be added is the project owner's decision.
- **Parts of phase 2b and phase 7 run beside phase 5** where they depend
  on nothing unfinished and will not be rewritten: step 1 of phase 2b
  and the libraries of phase 7.

### Still open

- **Failover**: the fixes for FO-01 to FO-16 are not committed; their
  complete integration run on 18.7 passed 937 tests and failed two (a
  test that the fix for FO-04 made wrong, corrected; and the check of a
  revoked certificate, which took a reset for something other than a
  refusal, corrected), and the run on 19rc2 has to be repeated: it was
  made while another suite ran, PostgreSQL did not start within its
  time, and every later test of the shared cluster failed.
  `TestInstanceMappingAfterAHandFailover` failed in two of five repeats
  on 19rc2 on the tree of 191bba73ee on a quiet machine and has passed
  sixteen of sixteen since; the cause was not found. Increment 3 (a
  failover started by hand) is written and has not passed a cluster
  test yet; increments 4 to 9 follow.
- **`kingres_shard`**: the high finding S12-01 of the fourth review of
  the call sequences is on both published branches; its fix is written
  and not integrated. Step 2 of phase 2b waits for it.
- **`kingres_columnar`**: `t/013_xid_wraparound` gave up once in the
  tree on each major without the table being frozen; the author found a
  band in the test (it waited for an age below 140 million while
  autovacuum acts only above 150 million) that explains the symptom and
  did not reproduce the failure in 44 runs; the change is being
  verified.
- **Reviews owed**: the fixes for L15 (`kingres_ledger`), C9
  (`kingres_columnar`) and A2 (anchoring), the libraries for keys and
  logs (running), step 1 of phase 2b, the fixes for ST-05 to ST-10. The
  fixes for L16 (verifier) are being written.
- **`kingresd` and the owner of a database**: it creates a database
  owned by the acting account or the requested owner, a role that is not
  a superuser, and such an owner can drop the database with its ledger.
  To be decided with step 6 of phase 7; `ledger_status` shows it.
- **The machine**: timing-sensitive suites fail when several suites run
  together (connections that time out, a server that does not start in
  its time). A complete integration run and a suite in the tree are not
  run at the same time any more.

### Core patches

None.

---

## 2026-11-17: kingresd stabilised, a write guard for the ledger, fix rounds and their reviews in four components, phase 2b and the libraries of phase 7 begun

### What was done

- **`kingresd`** (6c6725b1d1). The changes that end the failures of
  complete runs, together with the fixes for RBR-01 to RBR-07: the
  listener between servers has no write timeout; one lifecycle lock per
  data directory; the client between servers repeats calls that are safe to
  repeat; a `move_bucket` call is not cancelled once the extension recorded
  the move; a task that was fenced five times ends with the code `fenced`.
  Its review
  ([reviews/2026-11-17-kingresd-stabilisation-and-rebalance-second-fixes.md](reviews/2026-11-17-kingresd-stabilisation-and-rebalance-second-fixes.md),
  3d41a7f5e6): no critical finding, two high, three medium, five low,
  ST-01 to ST-10. ST-01 explains the failure that the last entry listed
  without a cause (a healthy group that refuses connections after another
  server was removed): when a group stops being enforced the lease file is
  no longer written, the watchdog is not told, and it stops the healthy
  primary.
- **`kingres_shard` 0.5**, three reviews and two fix rounds. The review
  of the S9 fixes
  ([reviews/2026-11-16-kingres-shard-0.5-second-sequences.md](reviews/2026-11-16-kingres-shard-0.5-second-sequences.md),
  b9415ab424) found one critical defect in the proof of a planned
  switchover; its fixes (30b6092d5f) brought a new proof (the new primary
  replayed the shutdown checkpoint of the old one as its last record) and
  a deadline on every call of a control plane. The review of those
  ([reviews/2026-11-17-kingres-shard-0.5-third-sequences.md](reviews/2026-11-17-kingres-shard-0.5-third-sequences.md),
  241846dd0b) found two critical and two high, the worst a regression of
  that round: the deadline stayed on for the rest of the transaction, so a
  later statement of the user to a node that did not answer in time was
  dropped with a warning and `COMMIT` succeeded (S11-01; on both published
  branches from 30b6092d5f until 4210c4a8a9). Its fixes (4210c4a8a9): the
  deadline is a depth around each control call that the data path never
  reads; a statement that was not carried out is an error; a lost session
  that carried writes of the user makes `COMMIT` fail; the previous secret
  is kept until every standby confirmed the current one (`KS014`). The
  review of those
  ([reviews/2026-11-17-kingres-shard-0.5-fourth-sequences.md](reviews/2026-11-17-kingres-shard-0.5-fourth-sequences.md),
  82191a0ab0) confirmed them and found one high, again introduced by the
  round (see "Still open").
- **The design of failover** ([design/ha-phase5-part2.md](design/ha-phase5-part2.md),
  c96cd73147) follows the new proof of a switchover.
- **`kingres_columnar` 0.1**, two reviews and two fix rounds: the sixth
  review
  ([reviews/2026-11-17-kingres-columnar-0.1-fifth-fixes.md](reviews/2026-11-17-kingres-columnar-0.1-fifth-fixes.md),
  df30ed5218) and its fixes (72aaa4d312: a write after the flush at commit
  goes through to the file, prepared buffers are never flushed, a ceiling
  on the memory of the write buffers, cleanup through the object access
  hook); the seventh review
  ([reviews/2026-11-17-kingres-columnar-0.1-sixth-fixes.md](reviews/2026-11-17-kingres-columnar-0.1-sixth-fixes.md),
  134d395ae7), with no wrong answer and no lost row and one high finding
  (a write while the transaction is ending stops the server), and its
  fixes (59d88c68ee).
- **The verifier and `kingres_ledger` 0.3, part A**, three reviews and
  two fix rounds. The third review
  ([reviews/2026-11-17-ledger-phase7-steps-1-2-second-fixes.md](reviews/2026-11-17-ledger-phase7-steps-1-2-second-fixes.md),
  9c03a08781) found that a member of `pg_write_all_data` forges a block.
  The fixes: the verifier (8d22b961e1: bounds on what it reads, exit
  status 4) and the extension (fe2e4d2ce2: the ledger tables are written
  by the code of the extension and by nothing else). The fourth review
  ([reviews/2026-11-17-ledger-phase7-steps-1-2-third-fixes.md](reviews/2026-11-17-ledger-phase7-steps-1-2-third-fixes.md),
  3eff168baa) found that a member of the role that owns the extension
  could call the internal write function. The fixes: the extension
  (1d4f742142: no function that SQL can call takes the content of a
  write; a write by hand leaves a record; `KL005`) and the verifier
  (777272e016: a stored state holds only what was verified, no size
  limit on a proof, the whole path of the state must be private, a
  verdict that could not be written is never status 0). The fifth review,
  of the extension
  ([reviews/2026-11-17-kingres-ledger-0.3-guard-second-review.md](reviews/2026-11-17-kingres-ledger-0.3-guard-second-review.md),
  1067b7498b): the rule for writes held; two high findings about what a
  role can stop or remove.
- **`kingresd` and the write guard** (0e48dc52d1). The library writes the
  ledger only through the functions of its own SQL version, so the API
  answered "forbidden" for a database on 0.1 or 0.2. It now checks the
  version before it enables the ledger on a table, makes a digest or
  records a verification, and answers 422 with the statement to run.
- **Phase 2b, step 1** (3cc9a270a4): one place computes a bucket from a
  value, the definitions that later steps need on both majors, and the
  comparison harness of the tests; no change of behaviour.
- **Phase 7, libraries for anchoring** (a3397d9baf): new packages of
  `kingresd` for AWS signatures, objects under S3 Object Lock, RFC 3161
  time stamps and OpenTimestamps, not yet used by `kingresd`.
- **Designs**: the phase 7 design gained corrections 20 to 48, the phase
  2b design the notes of step 1 (9ad7460cbd).

### How it was verified

Run by the integrator on the tree that was committed, not taken from the
reports of the authors, unless it says otherwise.

- `kingresd` at 6c6725b1d1: build for Windows and Linux, vet, format, the
  unit tests of 41 packages, the contract check; one complete run of the
  integration suite on each major, 675 passed, none failed or skipped.
- `kingres_shard`: at 30b6092d5f, out of tree with both installations
  named, 92 TAP files with 7670 tests on 18.7 and 8328 on 19rc2, in tree
  110 of 110; at 4210c4a8a9, 97 files with 7999 and 8657 tests, in tree
  118 of 118; at 3cc9a270a4, 13 regression tests and 99 files with 8730
  and 9389 tests, in tree 121 of 121 (with the suite of `kingres_ledger`).
  No compiler warning in any of them.
- `kingres_columnar`: at 72aaa4d312, in tree on 18 twice, 30 of 30, and
  on 19rc2 26 TAP files with 3695 tests; at 59d88c68ee, in tree twice, 34
  of 34, and on 19rc2 30 files with 4150 tests.
- The verifier: at 8d22b961e1, on Linux three runs of 210 top-level
  tests, on Windows three runs of 193 passing and 13 skipped; at
  777272e016, on Linux three runs of 282 (one skipped, a measurement), on
  Windows three runs of 251 passing and 15 skipped (tests for Unix). The
  committed extension out of tree with the verifier of 777272e016 named:
  612 tests on 18.7 and on 19rc2.
- `kingres_ledger`: at fe2e4d2ce2, in tree on 18 twice with the verifier
  named, 20 of 20, and on 19rc2 527 tests; at 1d4f742142, in tree twice,
  21 of 21, and on 19rc2 17 regression tests and 17 TAP files with 612
  tests.
- The ledger-related tests of `kingresd` (five packages and the cluster
  tests named Ledger or Alert) against the extensions of 1d4f742142 and
  4210c4a8a9 and the verifier of 777272e016, from an export of the
  committed control plane with the version check: 111 and 4 tests on 18.7,
  111 and 4 on 19rc2, none failing or skipped. Against fe2e4d2ce2 without
  the version check `TestInclusionProofOnLedger01` had failed on both
  majors, which is how the "forbidden" above was found. The complete
  integration suite was not run after 6c6725b1d1.
- The anchoring packages at a3397d9baf: format and vet clean; on Windows
  with the third-party vectors present the five packages pass; on Linux
  without them (what the commit holds) 124 top-level tests pass and 17
  skip, twice, and twice more with the race detector.
- The branch `kingres-19` after each synchronisation: built in the tree
  without a warning, the suites of the five extensions; at the last one
  (6879a6b0ab) 166 passed and one was skipped (the test that needs two
  installations).
- Shown by the authors and the reviewers only: that each new test fails
  without its fix; the mutants; the timings and memory figures; the fuzz
  runs; the checks of time-stamp tokens against openssl.

### Needs-verification items

- The statements listed as "to be verified" at the end of
  [design/ha-phase5-part2.md](design/ha-phase5-part2.md); four of them
  were settled by the author of the failover work, which is not committed
  yet.

### Decisions and deviations

- **Order of work.** Parts of phase 2b and of phase 7 that depend on
  nothing unfinished and will not need rewriting are written in parallel
  with phase 5 (decided with the owner of the project on 2026-11-17; before,
  2b was to wait for phases 4 and 5). Step 2 of phase 2b waits for the
  fixes of S12, because it changes the same connection code.
- `kingres_ledger`: a library that is newer than the SQL of a database
  refuses to write that ledger until `ALTER EXTENSION kingres_ledger
  UPDATE`; the capture triggers keep working. This departs from section
  9.5 of the design of phase 7 (correction 25). A dump does not carry a
  ledger; a physical backup or `pg_upgrade` does (correction 28).
- `kingres_columnar`: a write that reaches a columnar table while its
  transaction is ending is refused with a warning and the row is not
  written. The review of that round judged this the least bad behaviour
  for the commit, prepare and abort events, the release phases of a
  resource owner and a critical section (a heap table stops a server with
  assertions at nine of thirteen such moments, and its row written at the
  commit event is not durable), and found it too weak where the
  transaction is still alive (see "Still open", C8-02).
- Test vectors of other projects (the AWS signature test suite, the
  examples of the reference OpenTimestamps client) are not in the
  repository: a manifest of their sums and a script that fetches them from
  a pinned commit are, and the tests that need them skip without them.
  Whether to keep them in the repository under their own licences is an
  open question for the owner of the project.
- The verifier has a fifth exit status, 4, and no size limit on a proof.
- The control plane was changed by the integrator for the version check
  while another author had uncommitted work in the same directory; the
  four files were committed on their own and tested from an export of the
  committed tree.
- New work that could collide with a fix round is written in a git
  worktree of its own and brought to the branch as one commit.

### Still open

- **`kingres_shard`, on both published branches:** S12-01 (high,
  introduced by the fixes for S11): `remove_node`, `add_node`,
  `rotate_secret` and `sync_catalog` can return success with no warning
  although a node that went silent at the very end of the call did not
  get its part; a caller must read the result back. S12-03: the time
  budget per node ends a call against a healthy node that answers slowly,
  with `KS007`. S12-02 and S12-04 to S12-06. The fixes are being written.
  Partly done in the round before and stated in the README of the
  extension: S11-07, S11-08, S11-11.
- **`kingres_ledger`, on both published branches:** L11-01 (high): a
  member of `pg_maintain` or `pg_write_all_data` stops the builder and
  every capture with `LOCK TABLE` on a view over a ledger table. L11-02
  (high): a member of the role that owns the extension drops it, and with
  it the protection of every ledger table, through `DROP EXTENSION plpgsql
  CASCADE`. L11-03 to L11-05. Rows that alternate between two ledger
  tables cost 25 to 35 percent more CPU than before the guard. The fixes
  are being written. Rows forged before the guard existed cannot be told
  from real ones inside the database; the records of
  `ledger_remove_stray` and of writes by hand are covered by no hash.
- **`kingresd`, on both published branches:** ST-01 (do not set
  `defaultReplicaCount` to 0 and do not remove the last standby of a
  group on a cluster with enforcement on: the watchdog stops the healthy
  primary), ST-02 to ST-10. The first three increments of failover
  (standbys registered in the catalog, the instance mapping, the lease
  additions and read calls) and the fixes for ST-01 to ST-04 are written
  and being verified; two of their new cluster tests failed in some
  repeats on an overloaded machine and passed in all repeats on an idle
  one, and whether load alone explains it is not established. Failover
  itself, switchover and `restorePreferredPrimaries` are not written.
- **`kingres_columnar`, on both published branches:** the eighth review
  ([reviews/2026-11-17-kingres-columnar-0.1-seventh-fixes.md](reviews/2026-11-17-kingres-columnar-0.1-seventh-fixes.md))
  found no wrong answer and no lost row for a caller that does nothing
  PostgreSQL forbids, and one high, two medium and three low. C8-01
  (high): a truncation of a columnar table by the code of another module
  at the events at the end of a transaction is not guarded and ends in a
  PANIC, a backend that loops, or on 19 a table that cannot be read.
  C8-02: at the commit of a subtransaction and in a critical section the
  transaction is still alive, and the refused row is dropped with a
  warning while the transaction commits. C8-07: the random comparison
  test fails by itself about once in thirty runs. The fixes are to be
  written.
- The verifier: the fixes for L10 were not reviewed again; a second
  local account was simulated, not used, and nothing of the private path
  is checked on Windows.
- Not reviewed yet: step 1 of phase 2b; the anchoring packages, which
  were also never run against a real object store, a public time-stamp
  authority or the reference OpenTimestamps client.
- `kingres_sketch`, `kingres_timepart`: K12U-01 (documentation); the seal
  and the owner-driven path were not reviewed in depth.
- Phase 7: of 25 steps, the first two and the libraries of three more.
  Phase 2b: steps 2 to 13. Phases 9, 10 and 11 and the rest of phase 12
  are not started.
- The build machine: WSL failed twice on this day because the system
  drive of the host was full (the build cache of Go and the work areas of
  finished reviews); every run in flight was lost both times and
  repeated. No file of the repository was affected.

### Core patches

None.

---

## 2026-11-16 (second entry): Fix rounds and their reviews in every extension and in the verifier, the packages on PostgreSQL 19, the design of failover

### What was done

- **`kingres_shard` 0.5.** The fixes for S8 (35913a2901) and their review
  ([reviews/2026-11-16-kingres-shard-0.5-sequences.md](reviews/2026-11-16-kingres-shard-0.5-sequences.md),
  ed9a2623b6): ten findings, S9-01 to S9-10, one critical (a committed part
  rolled back after the retention time with no record) and two high. Their
  fixes (835b0967ff): a decision is kept until the participant confirmed
  durability; the WAL timeline of a promotion is recorded and checked;
  the previous secret is kept for outgoing sessions; `switchover_mark` and
  `switchover_verify`; `standby_name_matches`; and the chapter "What the
  control plane calls" rewritten with every answer of the nine sequences
  and the test that produces it.
- **`kingres_columnar` 0.1.** The fourth review
  ([reviews/2026-11-16-kingres-columnar-0.1-third-fixes.md](reviews/2026-11-16-kingres-columnar-0.1-third-fixes.md),
  2ccb0c3add; two critical) and its fixes (0051c754d2); the fifth review
  ([reviews/2026-11-16-kingres-columnar-0.1-fourth-fixes.md](reviews/2026-11-16-kingres-columnar-0.1-fourth-fixes.md),
  57b356d53b), the first without a critical finding (one high, one medium,
  four low), and its fixes (4cb965bca2).
- **`kingres_sketch` and `kingres_timepart`.** Three reviews and three fix
  rounds in one day:
  [third](reviews/2026-11-16-phase12-extensions-second-fixes.md) (b35416ef1e),
  fixed in fe25792c14 and 821c33be4b;
  [fourth](reviews/2026-11-16-phase12-extensions-third-fixes.md) (1e18418ab9),
  fixed in 89d79e7b84 and d1ef3851ee;
  [fifth](reviews/2026-11-16-phase12-extensions-fourth-fixes.md) (c19fbcc19c).
  What changed in substance: the history and the policies of
  `kingres_timepart` have one model of identity; a superuser's session no
  longer runs the maintenance for another role's table unless a setting
  says the owners are trusted, and an owner drives its own tables; a policy
  acts only when the interface sealed it. `kingres_sketch` bounds the
  memory of aggregate states with a ceiling that only a superuser sets.
- **The verifier and `kingres_ledger` 0.3, part A.** Their first review
  ([reviews/2026-11-16-ledger-phase7-steps-1-2.md](reviews/2026-11-16-ledger-phase7-steps-1-2.md),
  a51e662ede), the fixes (e4bfa4791e, 714eb98642), the review of the fixes
  ([reviews/2026-11-16-ledger-phase7-steps-1-2-fixes.md](reviews/2026-11-16-ledger-phase7-steps-1-2-fixes.md),
  33eb3314cf) and their fixes (c3305464bb, 7a558f2d51). The verifier opens
  every file through one helper that cannot block or leave a source
  directory, enforces the validity of keys, keeps the last verified
  checkpoint in a state file that only moves by a consistency proof, and
  escapes every string of its text output. The design of phase 7 gained
  corrections 14 to 19.
- **The review of the rebalance fixes**
  ([reviews/2026-11-16-kingresd-rebalance-fixes.md](reviews/2026-11-16-kingresd-rebalance-fixes.md),
  333a2313c0): thirteen of fourteen fixed, seven new findings, RBR-01 to
  RBR-07, two medium.
- **The packages on PostgreSQL 19** (6018981fa0): the package, image and
  compose tests give the same numbers on both majors; the four defects of
  the product that these tests had found are fixed.
- **The design of failover and switchover in the control plane**
  ([design/ha-phase5-part2.md](design/ha-phase5-part2.md), ce206868d1):
  the safety argument, the task step by step with every answer of the
  extension, the proposed contract 0.10.0, the pages of the console, the
  tests, ten increments.

### How it was verified

Run by the integrator on the tree that was committed, not taken from the
reports of the authors, unless it says otherwise.

- `kingres_shard` at 835b0967ff: out of tree with both installations
  named, 18.7: 12 regression tests and 84 TAP files with 7276 tests; 19rc2:
  7934 tests; in tree on 18, 102 of 102; no compiler warning. A first run
  of this round in the tree had stopped at one test; that led to the two
  defects named below.
- `kingres_columnar` at 4cb965bca2: in tree on 18 twice, 27 of 27; out of
  tree on 19rc2, 11 regression tests and 23 TAP files with 3494 tests.
- `kingres_sketch` at d1ef3851ee: in tree on 18 twice, 13 of 13; on 19rc2,
  8 regression tests and 9 TAP files with 559 tests. `kingres_timepart` at
  89d79e7b84: in tree twice, 8 of 8; on 19rc2, 14 regression tests and 4
  TAP files with 216 tests.
- The verifier at c3305464bb: on Linux three runs of 164 top-level tests,
  none failing or skipped; on Windows 154 passing and 6 skipped in two
  runs, and one run in which the temporary directory of one subtest could
  not be removed (thirty repeats then passed; cause not known).
  `kingres_ledger` at 7a558f2d51: in tree on 18 twice with the verifier
  named, 17 of 17; on 19rc2, 388 tests. The four tests of `kingresd` that
  check inclusion proofs with the verifier pass with it on both majors.
- The packages: the release build and the package of each major, then per
  major the package test under systemd (237 checks), the image test (130)
  and the compose test of three joined nodes (162), none failing; lintian
  and shellcheck clean.
- The branch `kingres-19` after each synchronisation: built in the tree
  without a warning, the suites of the five extensions; at the last one
  140 passed and one skipped (the test that needs two installations).
- Three times a run in the PostgreSQL tree failed a test that the authors'
  and the integrator's out-of-tree runs had passed, and each time
  something real was behind it: a test of `kingres_sketch` that assumed
  the library was preloaded, one that named the test database by a name it
  does not have in a tree build, and in `kingres_shard` a race between two
  equal timeouts that could answer one situation in two ways, with a
  recovery call behind it that waited for ever on a participant whose
  standby was away. The tree run stays part of every verification.
- Shown by the authors only: that each new test fails without its fix
  (the reviewers repeated it for most findings of the round before
  theirs); the repeats of new tests; the timings and memory figures; the
  fuzz targets of the verifier.

### Needs-verification items

- The twenty statements listed as "to be verified" at the end of
  [design/ha-phase5-part2.md](design/ha-phase5-part2.md).

### Decisions and deviations

- `kingres_timepart`: cleaning up after code of a table owner that ran in
  a superuser's session is not possible from SQL, so the protection is a
  refusal by default with an explicit setting to accept the risk, not an
  attempt at isolation.
- `kingres_sketch`: a first version of the memory limit (821c33be4b) could
  be raised by the role it bounds and made a hash aggregate quadratic in
  the number of groups (one million groups did not finish). Both were on
  the published branches for some hours on 2026-11-16 and are fixed in
  d1ef3851ee. The commit message of 821c33be4b claimed a protection that
  did not hold; it is corrected here and in the review, not in the
  message.
- The verifier: `not_after` and `not_before` of a key are enforced against
  the clock; a checkpoint the auditor already holds is not judged by the
  validity window of its key; an absent publication object is exit status
  3 and never 0.
- The design of failover departs from the design of phase 5 in one point:
  a forced failover without a majority is not in this part.
- Independent reviews are run one at a time from now on, not in parallel.

### Still open

- `kingresd`: not committed. The changes that were meant to end the
  failures of complete runs are in a working tree; two complete runs of
  the integration suite on 2026-11-16 (one per major, on a busy machine)
  each passed 652 tests and failed 3: on 18, a healthy group that refused
  connections for seconds after another server was removed, and a catalog
  that `check_cluster()` reports as differing, with a misplaced partition,
  after a task changed holder; on 19, a follower that refused a read
  during a short loss of the leader, and a call between servers that
  failed with a transport error. The causes are being examined together
  with RBR-01 to RBR-07. Failover, switchover and
  `restorePreferredPrimaries` are designed and not written.
- `kingres_shard`: the fixes for S9 were not reviewed again (in progress);
  a participant that requires no confirmation can lose its part after the
  retention time, recorded and not prevented.
- `kingres_columnar`: the fixes for C5 were not reviewed again.
- `kingres_sketch`, `kingres_timepart`: K12U-01 (documentation); the last
  review did not cover the seal and the owner-driven path in depth.
- The verifier and `kingres_ledger`: the fixes for L8 were not reviewed
  again; a member of `pg_write_all_data` can insert into the ledger
  tables, a valid successor block included.
- Phase 7: steps 3 to 25. Phase 2b, phases 9, 10 and 11 and the rest of
  phase 12 are not started.

### Core patches

None.

---

## 2026-11-16: The history of both branches rewritten once, rebalancing in the control plane, kingresd on PostgreSQL 19, phase 7 designed and begun, fix rounds and their reviews

### What was done

- **History rewritten, once, at the request of the project owner.** Two
  scripts of the control plane carried, in eleven early commits, a default
  value that was a path of the developer's machine. The line was replaced
  in those commits (the scripts now ask for `KINGRES_REPO` there), so every
  commit of `kingres-18` after `f78f85220f` has a new id: 75 commits. The
  trees of the last 64 of them are unchanged, and so are authors, dates and
  messages, except that a commit id cited in a message was replaced by the
  new id. The sync commits of `kingres-19` had their messages changed in
  the same way (24 commits, no tree changed). Both branches were
  force-pushed. Every commit id in the documents of the repository was
  replaced by its new id in this commit. An id noted outside the repository
  before this date no longer resolves. See
  [dev/releases.md](dev/releases.md), "History rewritten on 2026-11-16".
- **Phase 4: rebalancing driven by the control plane** (5196e3da5a,
  contract 0.9.0). One task per cluster: checks, a plan asked from the
  extension before every move, one bucket at a time, stop and start again,
  taken over after a restart or a leader change, a retry rule, a report
  with the placement before and after. The task engine gained the states
  paused and skipped. The Rebalance page of the panel. Defects recorded in
  the earlier entry were fixed with it: `distributeTable` with a reference
  table, `removeServer` counting standbys and a stopped rebalance,
  `wal_level` of the instances `kingresd` creates, the coverage of the
  ledger summary.
- **Its independent review**
  ([reviews/2026-11-16-kingresd-rebalance.md](reviews/2026-11-16-kingresd-rebalance.md),
  6b3a1b9847): fourteen findings, RB-01 to RB-14, two of them high and both
  about fencing: a holder that lost the lease of a task still ran the
  failure hook against the new holder's run, and the lease is not proved
  for the call that starts a move.
- **kingresd on PostgreSQL 19** (2aba6ea01e). `VerifyBinaries` accepts
  both majors since 5196e3da5a; the first complete run of the integration
  suite on 19 failed one test (two new settings of 19 had no reason of
  their own in the configuration policy). The installation of a run is
  selectable (`run-integration.ps1 -Install`, `KINGRES_INSTALL`).
- **Phase 7 designed**
  ([design/ledger-phase7.md](design/ledger-phase7.md), 3af21523d8): what of
  it exists, the decisions (signed checkpoints, tile logs per chain and for
  the aggregate, three anchor targets, epochs after a restore), 25 steps.
  **Step 1** (8ceed6b88b): the verifier checks signed notes, checkpoints,
  tile logs and consistency proofs and has a trust file; thirteen
  corrections to the design came out of building it against the
  specifications. **Step 2** (86e15fcae3): `kingres_ledger` 0.3 with
  `ledger_block_preimage`, `ledger_head` and `ledger_blocks_entries`; hash
  format 1 is unchanged.
- **`kingres_shard` 0.5, fixes for S7** (f47ce21bf5): the second phase of
  a commit is sent only when the commit record is confirmed; forced
  `synchronous_commit` on participants; identities of the deadlock
  detector from shared memory; a catalog term; a resumable `promote_node`;
  tagged names of prepared transactions. **Its review**
  ([reviews/2026-11-16-kingres-shard-0.5-fixes.md](reviews/2026-11-16-kingres-shard-0.5-fixes.md),
  f2ba3d95e4): eight findings, S8-01 to S8-08, one high (the durability
  check protects the origin and not a participant) and three medium; a
  verdict on each call sequence the control plane must follow.
- **`kingres_columnar`**: fixes for C2 (87611197d3); a third review
  ([reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md](reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md),
  1069c409f3) with six findings, C3-01 to C3-06, one critical (the
  SERIALIZABLE fix did not hold for a stripe flushed inside a
  subtransaction, and the tests that should have caught it did not flush);
  fixes for C3 (bb4519e787).
- **`kingres_sketch` and `kingres_timepart`**: the review of their fix
  rounds
  ([reviews/2026-11-15-phase12-extensions-fixes.md](reviews/2026-11-15-phase12-extensions-fixes.md),
  6d539e84a5), three findings, and their fixes (f33498c9a5): the policy
  and history tables of `kingres_timepart` are reached through
  security-barrier views instead of row-level security, so a backup role
  that is not a superuser can dump the database.

### How it was verified

Run by the integrator on the tree that was committed, not taken from the
reports of the authors, unless it says otherwise.

- The rewrite: the tree of the new tip of each branch is the tree of the
  old tip; of the 75 commits of `kingres-18`, 64 have an unchanged tree
  and 11 differ in that one line; author, committer and both dates of
  every commit are unchanged; no blob and no message of the new history
  contains the path; every id cited in a message or a document resolves to
  a commit of the new history. The old tip is kept under a local backup
  reference that is not published.
- `kingresd` at 2aba6ea01e, with `kingres_shard` of f47ce21bf5 and
  `kingres_ledger` 0.3 installed into both test installations: one
  complete integration run on each major, 619 top-level tests, none
  failing, none skipped, on PostgreSQL 19rc2 and on 18.7.
- Phase 4 at 5196e3da5a on PostgreSQL 18: 617 of 618 top-level tests
  passed in the complete run; the one that failed (leaving and joining
  again) passed six of six alone; the three-server smoke script, every
  check passed; the panel: 1010 unit tests, 43 browser tests, the
  end-to-end run against a real `kingresd`, 50 of 50 steps.
- `kingres_shard` at f47ce21bf5: out of tree on 18.7, 12 regression tests
  and 69 TAP files with 6858 tests; on 19rc2, 7516 tests; in tree on 18,
  86 of 86 suites; no compiler warning.
- `kingres_columnar` at bb4519e787: in tree on 18 twice, 19 of 19; out of
  tree on 19rc2, 11 regression tests and 15 TAP files with 2949 tests.
- `kingres_ledger` 0.3 at 86e15fcae3: in tree on 18 twice with the
  verifier named (its checks ran on the output of the extension); on
  19rc2, 374 tests. The verifier at 8ceed6b88b: 103 top-level tests.
- `kingres_sketch` and `kingres_timepart` at f33498c9a5: in tree on 18
  twice; on 19rc2, 348 and 134 tests.
- The branch `kingres-19` after each synchronisation: built in the tree
  without a warning, the suites of the five extensions; at the last one,
  113 passed and one skipped.
- Shown by the authors only: that each new test fails without its fix;
  the smoke script and the end-to-end run of the panel on PostgreSQL 19;
  the fuzz targets of the verifier.

### Needs-verification items

- The statements of section 13 of the phase 7 design, except the three
  that step 1 settled (section 14 of the design).

### Decisions and deviations

- The rewrite of history and the force-push of both branches, by decision
  of the project owner. The rule of [dev/releases.md](dev/releases.md)
  that `kingres-19` is never force-pushed by a sync stands; this was not a
  sync.
- The commit message of the phase 4 work claimed that a superseded leader
  can neither record nor start a move; the review showed that it can still
  start one. The commit message of the second fix round of
  `kingres_columnar` claimed a coverage of flushes by size that the tests
  did not have. Both are corrected in the reports and in the README of
  the repository, not in the messages.
- Defaults chosen in the phase 7 design that the project owner may
  change: publication off, ledger tables on one shard group, no anchor
  target on by default, governance mode for write-once storage.

### Still open

- `kingresd`: the findings RB-01 to RB-14 (a fix round is in progress and
  is not committed); failover, switchover and
  `restorePreferredPrimaries` are not written.
- `kingres_shard`: the findings S8-01 to S8-08 (a fix round is in
  progress and is not committed).
- `kingres_columnar`: the fixes for C3 were not reviewed again; five
  tests that the third review asked for are not written.
- `kingres_sketch`, `kingres_timepart`: the fixes for K12R were not
  reviewed again.
- The package tests were not run on PostgreSQL 19 by the integrator.
- Phase 7: steps 3 to 25. Phase 2b, phases 9, 10 and 11 and the rest of
  phase 12 are not started.
- Copies of the old history that were fetched before the rewrite, and the
  hosting service's own cache of unreachable commits, are outside the
  repository's control.

### Core patches

None.

---

## 2026-11-15: One source for PostgreSQL 18 and 19 and two product branches, kingres_shard 0.4 and 0.5, three new extensions, high availability part 1, configuration from the console, AGPL

### What was done

- **One extension source for two majors**
  ([ADR 0023](adr/0023-one-extension-source-for-several-majors.md), commit
  721ff7f034). Every extension builds and passes its tests on PostgreSQL
  18 and 19 from one source; the differences between the majors live in
  one header per extension (`src/pg_compat.h`). A cluster runs one major.
  `kingres_ledger` was ported first (3c04aad2b8), then `kingres_shard`
  (9944f08d01); the three new extensions were written that way from the
  start.
- **Two product branches** ([dev/releases.md](dev/releases.md), commits
  fd834804e3, 23ba13bd2b, ec7d6b1a3e): `kingres-18` on `REL_18_7`, where
  development happens, and `kingres-19` on `REL_19_RC2`, which is the
  upstream tag, the core patches and one "sync from kingres-18" commit per
  synchronisation, so that the KingresDB directories of the two branches
  are byte-identical. Both are published. Releases will be named
  `kingres-<version>-pg18` and `kingres-<version>-pg19`.
- **`kingres_shard` 0.4** (37293a7e77): the fix round for the reviews of
  0.2 and 0.3 (S3-01 to S3-18, S4-01 to S4-10). Later versions moved up by
  one (b59008bb1b). Its independent review
  ([reviews/2026-11-15-kingres-shard-0.4.md](reviews/2026-11-15-kingres-shard-0.4.md),
  d29a408a7d, findings S5) was answered together with the port to
  PostgreSQL 19 (9944f08d01); the review of the port
  ([reviews/2026-11-15-kingres-shard-0.4-port.md](reviews/2026-11-15-kingres-shard-0.4-port.md),
  7cfde8ebd0, findings S6-01 to S6-10) was answered in 0ec398f6a2, which
  also lets the extensions be created in any order (an extension that
  finds the schema `kingres` already made by another no longer fails).
- **`kingres_shard` 0.5** (5cc5f078db): the part of phase 5 that belongs
  to the extension. Standby nodes in the catalog, `kingres.promote_node`,
  prepared transactions that survive a failover (the name of a prepared
  transaction carries the group, the timeline, the backend and the
  target), distributed deadlock detection, `kingres.create_restore_point`.
  Its independent review
  ([reviews/2026-11-15-kingres-shard-0.5.md](reviews/2026-11-15-kingres-shard-0.5.md),
  bb0f2813d9) has ten findings, S7-01 to S7-10: one critical (the second
  phase of a commit is sent after a cancelled wait for the synchronous
  standby) and six high. The README says that failover is not to be
  relied on yet.
- **`kingres_sketch` 1.0 and `kingres_timepart` 0.1** (5fc2a70fad), the
  parts of phase 12 that depend on nothing else: approximate distinct
  count, top-n and approximate percentiles as types with aggregates that
  support partial aggregation; creation of time partitions ahead of time
  and retention for ordinary partitioned tables. Their review
  ([reviews/2026-11-15-phase12-extensions.md](reviews/2026-11-15-phase12-extensions.md),
  findings K12-01 to K12-17) was answered in e6ee42f265 and 873cba63ce.
- **`kingres_columnar` 0.1** (b25c43c884): columnar storage as a table
  access method, with compression, skipping by minimum and maximum, and a
  custom scan that reads only the columns a query needs. Two independent
  reviews:
  [the first](reviews/2026-11-15-kingres-columnar-0.1.md) (4988395b25,
  C1-01 to C1-14) was answered in 5555b252a0, which changed the on-disk
  format; [the second](reviews/2026-11-15-kingres-columnar-0.1-fixes.md)
  (9e19a38979) confirmed those fixes and has nine new findings, C2-01 to
  C2-09, one of them critical (under SERIALIZABLE a write that a
  concurrent reader cannot see is not detected as a conflict). The README
  warns against real use.
- **High availability, part 1, in `kingresd`** (acebb486b6, contract
  0.7.0): heartbeats and liveness of servers, leases with self-fencing
  (enforced only in a group that has a standby), the watchdog
  `kingres-watchdog`, creation and supervision of standbys, the client
  entry point. Two defects found by the three-node package test were
  fixed in d05808e9ff: a primary started before it held its lease and was
  stopped by its own fence at the first failed renewal (it now waits for
  the lease at a cold start), and an orderly stop of all instances could
  be taken for a lost lease (the stop now stands the watchdog down).
  Failover and switchover themselves are not written.
- **Phase 13, configuration from the console** (design 2604fcaaab,
  implementation 1a7a04f3c2, contract 0.8.0 with 82 paths and 100
  operations): the settings of PostgreSQL and the time zone, read and
  changed for the whole cluster, a server group or one server, with a
  preview, a history, reverting a change, and a restart of the instances
  that wait for one. `kingresd` writes the settings it manages into a file
  of its own that the instance includes.
- **Web console**: the top bar is one black row in both themes, chart
  legends, aligned form rows, a footer on every page with the copyright,
  the link to the project site, the licence and a link to the source of
  the running version, the horizontal logo on the sign-in page
  (2f4ab3bbbf and later); the pages of contract 0.7.0 (d546dcc06d) and
  the configuration page of 0.8.0 (1a7a04f3c2).
- **Packaging** (e48d2e4ec4, 240c26897f, bad3732c05, 3e04d655bf): the
  watchdog is shipped; the image, the compose file and the Debian package
  are built for either major (`kingres_<version>+pg18-<rev>_amd64.deb`,
  `kingres:<version>-pg18`, and the same for 19); the package of one major
  refuses to install over the data of the other; the licence metadata
  says AGPL-3.0-only.
- **Licence** ([ADR 0024](adr/0024-agpl-for-kingresdb-code.md),
  daa471e852): KingresDB's own code is `AGPL-3.0-only`; the code of
  PostgreSQL keeps the PostgreSQL License; the two files that adapt
  PostgreSQL code say `AGPL-3.0-only AND PostgreSQL`. Every file of
  KingresDB carries the identifier. ADR 0004 is superseded in its choice
  of licence.
- **Roadmap** (7fcbc2e6ad): the first release is every phase except
  phase 8 (several map groups and geo-distribution), which moves to the
  future roadmap. Nothing is tagged before all of them are finished. The
  design of phase 2b was revised against `kingres_shard` 0.4 (5754aa4f5b).

### How it was verified

Run by the integrator on the tree that was committed, not taken from the
reports of the authors, unless it says otherwise.

- `kingres_shard` at 0ec398f6a2: out of tree on PostgreSQL 18.7, 12
  regression tests and 60 TAP files with 6610 tests; on 19rc2, the same 12
  and 60 files with 6716 tests; no compiler warning with `-Werror` on
  either; in the tree on 18, 75 of 75 suites. No test of the version of
  the server outside the compatibility header.
- The branch `kingres-19` after the synchronisation at 0ec398f6a2: built
  in the tree with no warning; the suites of the five extensions, 95
  passed and one skipped (`t/046_two_majors`, which runs only when both
  installations are named).
- `kingres_ledger`: 281 subtests on 18 and 280 on 19. `kingres_sketch`:
  339 subtests on 19. `kingres_timepart`: 105 on 19. Each also in the tree
  on 18.
- `kingres_columnar` at 5555b252a0: on 19rc2 out of tree, 11 regression
  tests and 8 TAP files with 993 tests; in the tree on 18 twice, 12 of 12
  suites.
- `kingresd` at 1a7a04f3c2: build for Windows and Linux, `go vet`,
  `gofmt`, unit tests; the integration run against real instances of
  PostgreSQL 18: 40 test binaries, 594 top-level tests, no failure, none
  skipped; the three-server smoke script, 20 of 20 steps. The contract
  checker passes on 0.8.0.
- Web console at 1a7a04f3c2: type check, lint, 998 unit tests (passed
  twice at normal load; one to three tests that poll failed in three runs
  while the machine was saturated by other work), 43 browser tests,
  production build, generated-API and brand checks, and the end-to-end run
  against a real `kingresd`, 50 of 50 steps.
- Packaging at 5555b252a0 on PostgreSQL 18: the image test, 129 passed and
  one known failure; the compose test of three nodes, 162 passed; the
  Debian package, 202 passed and one known failure, `lintian` without an
  error. The check that the package of the other major is refused was run
  by its author only, and so were all package tests on PostgreSQL 19.
- The reviews ran on an export of the committed tree, by a reviewer who
  had not seen the work; what each could not cover is listed at the end of
  its report.

### Needs-verification items

- "PostgreSQL 19 is released on 2026-11-19": the date is the announced
  one; `kingres-19` stays on `REL_19_RC2` until the tag `REL_19_0` exists.
- The statements the new design documents label as unchecked stay
  unchecked.

### Decisions and deviations

- [ADR 0023](adr/0023-one-extension-source-for-several-majors.md) and
  [ADR 0024](adr/0024-agpl-for-kingresdb-code.md), both at the request of
  the project owner.
- **No check runs on GitHub.** GitHub Actions is switched off for the
  repository and no workflow of KingresDB is published; every check runs
  on the developer's machine ([dev/releases.md](dev/releases.md)). This
  replaces the open item "CI workflow" of the earlier entries.
- **No tag before the first beta**, and no tag of upstream PostgreSQL is
  pushed to the repository of KingresDB.
- **Version numbers of `kingres_shard`**: 0.4 is the fix round, 0.5 the
  high-availability part, 0.6 phase 2b, 0.7 to 0.9 phases 9 to 11. The
  design documents written before this still show the older numbers; the
  version paragraph of [architecture.md](architecture.md) is the
  reference.
- **Order of work**, set by the project owner: phases 5 and 4 are
  finished first, then phase 2b, then phases 9, 10, 11, the rest of 12 and
  13, then phase 7.
- A lease is enforced only in a group that has a standby, as decided in
  the previous entry; it is implemented that way.

### Still open

- `kingres_shard` 0.5: the findings S7-01 to S7-10. A fix round is in
  progress and is not committed. The fixes of S6 were not reviewed again.
- `kingres_columnar`: the findings C2-01 to C2-09. A fix round is in
  progress and is not committed.
- `kingres_sketch` and `kingres_timepart`: the fix rounds of K12 were not
  reviewed again.
- `kingresd`: failover, switchover and `restorePreferredPrimaries` are
  not written; the six rebalance operations are in progress and are not
  committed. `kingresd` accepts only the binaries of PostgreSQL 18
  (`VerifyBinaries`), so the integration suite was never run on
  PostgreSQL 19 and the package tests fail there.
- Phase 2b, phases 9, 10 and 11, the rest of phase 12 (repartitioned
  joins, `MERGE`, columnar storage for distributed tables, time
  partitioning for distributed tables and its scheduler, pushdown of
  sketches) and phase 7 are not started.
- The contract and control-plane defects listed under "Still open" of the
  previous entry are part of the rebalance work in progress.
- `kingres-19` must be moved to `REL_19_0` when upstream tags it; that
  push changes an upstream workflow file and needs a credential with the
  permission for it.

### Core patches

KP-0003 (`contrib/meson.build`, now five `subdir` lines: columnar,
ledger, shard, sketch, timepart) and KP-0004 (`LICENSE`, a new file at
the top of the tree with the text of the GNU Affero General Public
License, version 3; the `COPYRIGHT` file of PostgreSQL is not changed).
Register: [dev/core-patches.md](dev/core-patches.md).

---

## 2026-11-15: Contract 0.6.0, the fix round of the second kingresd review, the panel through 0.6.0, kingres_shard 0.3 and its review, phases 9 to 12 added to the roadmap

### What was done

- **Control plane, fix round for the second review** (commit 8d1b2dfc04;
  findings CP2-01 to CP2-22 of
  [reviews/2026-11-15-kingresd-0.4.md](reviews/2026-11-15-kingresd-0.4.md)).
  A forwarded request now carries a proof of the caller's identity that
  only the manager leader can make, per request; task leases use the clock
  of the system database and carry a fencing epoch; the audit log is read
  cluster-wide; the rate limiter no longer counts successful sign-ins; a
  server whose internal API is switched off answers `internal_api_disabled`.
- **REST contract 0.6.0** (same commit), 71 paths and 88 operations. Two
  breaking changes: `addServer` needs `kingres_admin`, and `addServer` and
  `joinCluster` without a fingerprint pin are refused with
  `validation_failed` unless the request says `allowUnpinned: true`.
  Additive: `internalFingerprint` on the state and the local-server
  answers, `complete` and `uncoveredShardGroupIds` on ledger digests and
  verifications, `AuditEvent.serverId`, partial answers of the audit log,
  the alert kind `replicationPending`, `LedgerProofBlock`, a degraded
  session (`Session.degraded`, `degradedReason`, `clientAddress`), a dry
  run of `replaceNetworkAccess`.
- **Web panel**: an independent review
  ([reviews/2026-11-15-web-panel-0.5.md](reviews/2026-11-15-web-panel-0.5.md),
  findings W2-01 to W2-16, commit d349e59483); its fixes together with the
  certificate actions and the display of incomplete cluster-wide answers
  (39652843fc); then contract 0.6.0 (e64277c568): choosing or refusing a
  pin when a server is added or joins, the server's own fingerprint, ledger
  coverage, the degraded session, the dry run of network access. The copy of
  the session in `sessionStorage` that 39652843fc introduced was removed
  again in e64277c568; the browser stores the theme only.
- **Packaging**: the bucket count reported by a follower is an ordinary
  check of the three-node test again (053b7a3437).
- **`kingres_shard` 0.3** (6ae61063eb, design
  [design/shard-phase4.md](design/shard-phase4.md)): `kingres.move_bucket`
  (copy, logical replication with a marker row that proves the copy is
  complete and replayed, a two-phase switch that detaches on the source and
  attaches on the target), `kingres.rebalance_plan`,
  `kingres.check_cluster`, `kingres.set_node_property`. Scans and fanned-out
  writes compare a hash of the placement before and after they open their
  cursors and fail with the retryable `KS001` when it changed.
- **A defect that was already in 0.2**, found while verifying 0.3: the
  recovery worker sent its two `SET` commands as one statement of the
  extended protocol, which PostgreSQL refuses, so its remote sessions had no
  statement or lock timeout. Fixed in `src/recovery.c` (one statement each;
  a node whose session cannot be bounded is skipped and logged), with a
  check in `t/022_timeouts_and_worker.pl` that the logs of the nodes do not
  hold the refusal.
- **Independent review of `kingres_shard` 0.3**
  ([reviews/2026-11-15-kingres-shard-0.3.md](reviews/2026-11-15-kingres-shard-0.3.md),
  commit 3c0f21b1a9): ten findings, S4-01 to S4-10. Three are serious and
  were reproduced: a bucket that moves away and back while a scan waits
  leaves the placement hash unchanged, and the scan returns an incomplete
  answer without an error (S4-01, critical); the recovery worker neither
  resumes nor cleans an interrupted move (S4-02); a write made directly into
  a bucket partition is not covered by the marker proof (S4-03).
- **Roadmap**: phases 9 to 12 were added to
  [architecture.md](architecture.md) after a comparison with the
  documentation of Citus 13.0: table lifecycle (distributing a table with
  rows, undistributing, changing the distribution column or the bucket
  count, partitioned tables), elasticity (bucket split, background
  rebalancing with strategies, draining a server), distributed schemas, and
  real-time analytics (repartitioned joins, `MERGE`, approximate aggregates,
  columnar storage, time partitioning with retention). Each has its design
  document, linked from the roadmap table.
  [ADR 0022](adr/0022-the-complete-scan-stays.md) records the rule these
  phases are built under: the row-moving distributed scan stays as the path
  that answers every query.

### How it was verified

Run by the integrator on the tree that was committed, not taken from the
reports of the authors, unless it says otherwise.

- `kingresd` at 8d1b2dfc04: build for Windows and Linux, `go vet`, `gofmt`,
  unit tests of 32 packages; the integration run in WSL against real
  instances: 32 test binaries, 456 top-level tests, no failure, none
  skipped. The three-server smoke script was run by its author only.
  `TestFanoutCallsEveryServerInParallelWithItsOwnDeadline` failed once
  under load; its deadlines were too short for a loaded machine and were
  lengthened (30 repeats passed afterwards).
- Contract: `scripts/check-contract.mjs` passes on 0.6.0.
- Web panel at e64277c568: type check, lint, 577 unit tests, 22 browser
  tests, production build, generated-API and brand checks. The end-to-end
  run against a real `kingresd` (45 of 45) was run by its author only.
- Packaging at 8d1b2dfc04: the compose test of three nodes joined into one
  cluster, 139 checks passed and one known failure (a stopped follower is
  still listed as healthy: there are no heartbeats before phase 5).
- `kingres_shard` 0.3 at 6ae61063eb: out of tree, 12 regression tests and
  32 TAP files with 5151 subtests; in the tree twice, 47 of 47 suites, no
  compiler warning; `t/022` thirty times under CPU load without a failure.
  Before the fix of the worker and of the test, `t/022` failed 2 of 7 runs
  under load.
- The review of 0.3 ran the whole suite once (35 of 36; `t/027` failed
  under load and passed alone) and did not cover `COPY`, user two-phase
  transactions, or long `REPEATABLE READ` transactions across a switch.
- The design documents of phases 9 to 12 are designs: nothing in them was
  built or run.

### Needs-verification items

None of the items of `architecture.md` was checked in this unit of work.
The new design documents label their own unchecked statements.

### Decisions and deviations

- Lease loss stops a primary only in a group that has a standby; in a group
  without one it only raises an alert (decided for the phase 5 work in
  progress; to be recorded in the phase 5 design when that work lands).
- Work on `contrib/kingres_shard` is done by one line of work at a time;
  parallel changes to it conflicted.
- [ADR 0022](adr/0022-the-complete-scan-stays.md), at the request of the
  project owner.

### Still open

- The findings S3-01 to S3-18 and S4-01 to S4-10 of `kingres_shard`: a fix
  round is in progress and is not committed. Until S4-01 to S4-03 are fixed
  a bucket move is not safe for real use.
- `kingresd`: the six rebalance operations, `failoverServer` and
  `restorePreferredPrimaries` are not implemented (80 of 88 operations
  are); the fencing epoch is not yet sent to remote callees; ledger digests
  cover one shard group each. Phase 5 part 1 (heartbeats, leases, standbys,
  the client proxy port) is in progress and is not committed.
- Phase 2b and phases 9 to 12 are designs only. The five new designs each end
  with a list of the statements that still need verification (14, 14, 17,
  17 and 27 of them), and the points that cross
  phase boundaries are listed under the roadmap table in
  [architecture.md](architecture.md).
- Defects in the existing code and contract that the design work found, not
  fixed yet:
  - the contract says `distributeTable` moves rows and that a distributed
    table can be a ledger table at the same time; the extension needs an
    empty table and refuses a ledger table;
  - `kingresd` answers `distributeTable` with `method: reference` as not
    supported, although the extension has `create_reference_table`;
  - `removeServer` counts the standbys a server hosts as buckets it owns,
    so with chained placement no server can be removed before its standbys
    are relocated, which the contract does not say;
  - the task engine declares the state `paused` but nothing sets it, and a
    stopped rebalance is counted as running;
  - `create_distributed_table` does not look at the access method of the
    table, and `ledger_enable` does not either;
  - `choose_colocation` takes `max(colocation_id) + 1`, which can reuse the
    number of a dropped group;
  - `COPY <table> TO` of a distributed table was never examined and probably
    returns the local partitions only;
  - a `SELECT` that reads a distributed table of another shard group
    together with a local table is refused, which ADR 0022 does not allow
    for reads;
  - DDL runs on the primary of the map group only, so a client that creates
    schemas or tables through another server is refused.
- The CI workflow is not in the repository: the access token in use cannot
  push workflow files.

### Core patches

None.

---

## 2026-11-15: Contract 0.4.0 complete and 0.5.0, certificates, cluster-wide reads, a real three-node cluster test, second review of kingresd

### What was done

- **Control plane, 80 of 88 operations** (commits a51d630d53 and
  f6f00796fe):
  - the rest of contract 0.4.0: server groups, inclusion proofs, the list of
    ledger verifications, start, stop and restart of an instance, leaving a
    cluster, local settings; the security headers and the cookie attributes
    the panel's review asked for; crash points in the task engine with a
    test that interrupts a join at every step;
  - certificates: every server renews its own certificate when a third of
    its lifetime is left, the key stays, nothing is restarted, and the
    revocation list is issued again long before it runs out; an
    administrator can renew or revoke a server's certificates;
  - `listLogs`, `listAlerts` and `queryMetrics` answer for the whole cluster
    from any server and name the servers that did not answer;
  - four alerts are raised: `certificateExpiring`,
    `certificateRenewalFailed`, `ledgerPendingTooOld`, `versionMismatch`.
- **REST contract 0.5.0**, additive only: the two certificate operations,
  `partial` and `missingServers` on the log, alert and metric answers, two
  problem codes, two task kinds, one alert kind.
- **Web panel** (commit 36af22e61f): follows every entry of the 0.4.0
  changelog (ledger proof and byte-exact digest download, fingerprint pin
  when adding or joining a server, instance start and stop, local settings,
  leave cluster, server groups), shows one calm state while the system
  database is stopped, and is generated from contract 0.5.0.
- **Packaging** (commit 16da482e03): the compose test joins the three nodes
  into one cluster through the REST API, once with `addServer` and once with
  `joinCluster`, both with the certificate fingerprint pinned, and then
  writes on one node and reads on all three. The compose file had the
  internal API on a port no peer looks at; it is corrected.
- **Second independent review of `kingresd`**
  ([reviews/2026-11-15-kingresd-0.4.md](reviews/2026-11-15-kingresd-0.4.md)):
  22 findings, none critical, four high.

### How it was verified

Run by the integrator on the committed tree unless it says otherwise.

- Control plane: Windows and Linux builds, `go vet`, `gofmt -l`, unit tests
  of 30 packages, and the WSL integration run with real PostgreSQL: 30 test
  binaries, 391 top-level tests, none failed, none skipped. The race
  detector was run by the author only, on Windows, without PostgreSQL.
- Contract 0.5.0: the rule checker passes (71 paths, 88 operations, 172
  schemas).
- Panel: typecheck, lint, build, generated-files check and brand check
  clean; 396 unit and component tests; 17 tests in a headless browser. The
  end-to-end run against the real server (44 steps, including a proof
  checked by `kingres-verify`) was run by its author only.
- Packaging: the compose cluster test from a clean snapshot of the commit,
  138 checks, none failed, two marked as known defects of `kingresd` (below).
  The image and package tests were not run again by the integrator after
  this change; the author ran the image test (79 checks).

### Decisions and deviations

- A renewed certificate keeps the server's key and names, and the previous
  certificate stays valid until it expires. The CA therefore records every
  serial it issues, and a revocation covers all of them.
- A revoked server gets back in by being removed and joining again. That
  works only for a server that holds no bucket copy until rebalance exists.
- Alerts stay on the server that raised them; an alert id carries the
  server's ordinal and stays below 2^53.
- Cursors of logs and alerts changed format with 0.5.0; a cursor from 0.4.0
  answers 400.
- The panel's unit tests run with six workers: two timer-dependent tests
  failed now and then with one worker per core.

### Still open

- **`kingresd`, second review**: a forwarded identity is accepted by name
  from any holder of a cluster certificate (CP2-01); `addServer` lets
  `kingres_ops` reach `kingres_admin` (CP2-02, by reading only); a pinned
  fingerprint is dropped when `addServer` goes through a follower (CP2-03);
  task leases use the claiming server's clock (CP2-06); ledger verification
  covers the leader's shard group only but answers `passed` (CP2-08). The
  fix round is running. CP2-04 (no renewal) is closed by the certificate
  work above.
- **`kingresd`, found by the compose test**: `getDatabase` on a follower
  answers a null bucket count, and a stopped follower stays listed as live
  because there is no heartbeat (phase 5).
- **Eight operations** still answer "not supported yet": the six of
  rebalance, `failoverServer` and `restorePreferredPrimaries`.
- **`kingres_shard` 0.3** (moving buckets) is written but not committed: one
  of about fifteen thousand scans during a move returned a wrong count
  without an error. The cause is being looked for. The fix round for S3-01
  to S3-18 has not started.
- **Panel**: no buttons for renewing or revoking certificates and no notice
  for incomplete cluster-wide answers yet; both are being built. An
  independent review of the fix round and of the 0.4.0 change is running.
- A file pushed in commit 57b8924ca2 contained a machine-local path; the
  file is corrected, the line remains in history.

---

## 2026-11-15: Several servers in one cluster, four independent reviews and their fix rounds, contract 0.4.0

### What was done

- **Control plane, several servers**
  ([design/control-plane-3.md](design/control-plane-3.md)): cluster
  operations are forwarded to the manager leader; a server joins a cluster,
  and servers are added and removed, with certificate trust between the
  instances (`hostssl` with client certificates mapped through
  `kingres_nodes`). Seven mismatches with the contract found by the panel's
  end-to-end run were fixed.
- **Independent reviews**, each by a reviewer who did not write the code and
  who had to reproduce what was claimed:
  - `kingres_shard` 0.2
    ([reviews/2026-11-14-kingres-shard-0.2.md](reviews/2026-11-14-kingres-shard-0.2.md)),
    then a second review of its fix round
    ([reviews/2026-11-14-kingres-shard-0.2-fixes.md](reviews/2026-11-14-kingres-shard-0.2-fixes.md));
  - `kingresd`, as a security review
    ([reviews/2026-11-14-kingresd-control-plane.md](reviews/2026-11-14-kingresd-control-plane.md),
    findings CP-01 to CP-07, two of them critical);
  - the web panel
    ([reviews/2026-11-14-web-panel.md](reviews/2026-11-14-web-panel.md),
    findings W-01 to W-18, none critical).
- **Fix rounds**, every fix with a test that fails without it:
  - `kingres_shard` 0.2: the findings of the first review (the recovery
    ordering rule, comparison of session settings, serialized writers of
    reference tables, authentication of forwarded sessions with a token
    derived from a cluster secret, bounded waits).
  - `kingres_ledger` 0.2 and the verifier: the eight findings of the review.
  - `kingresd`: CP-01 to CP-07. Connection strings are built in one place
    that quotes every value; the roles of a session are derived again on
    every request and a role change ends that role's sessions on every
    server; failed statements are not written to the log; workbench results
    are bounded; dangerous memberships and attributes are refused.
  - Web panel: W-01 to W-18, plus nine defects found in pages the review
    had not examined.
- **REST contract 0.4.0** (first part): reading logs needs `kingres_ops`,
  `Task.detail`, `expectedFingerprint` on the join and add-server requests,
  the `replicate.apply` task kind, three alert kinds.
- **Packaging** for the `kingresd` that manages instances: the unit is
  `Type=notify` with `KillMode=process`, so restarting the manager leaves
  the database running; the three artifact tests now create a real cluster
  through the API and restart it in every mode.
- **Brand**: the project's logos are used in the panel (sign-in page, top
  bar, favicon) and in the top-level README; the sources are in
  `kingres/brand`.
- **Design**: phase 5, high availability
  ([design/ha-phase5.md](design/ha-phase5.md)), with
  [ADR 0021](adr/0021-failover-in-kingresd.md): failover is part of
  `kingresd` and is fenced by leases granted by a majority of servers;
  Patroni and etcd are not shipped. Phase 2b of sharding
  ([design/shard-phase2b.md](design/shard-phase2b.md)): pushdown of
  aggregates, sort and limit, joins of colocated tables, and row routing
  for bulk writes.
- **Architecture document**: a table near its top lists where the
  implementation departs from the original design, with the ADR of each.

### How it was verified

Everything below was run by the integrator on the tree that was committed,
unless it says otherwise.

- `kingres_shard` 0.2 after its fix round: 11 pg_regress tests and 4405 TAP
  subtests in 24 files out of tree, and both extension suites in tree (39 of
  39). The second reviewer reverted 17 fixes one at a time and saw the
  guarding test fail each time, and ran 16 crashes under load with no
  half-committed and no lost acknowledged transaction (reviewer's run).
- `kingres_ledger` 0.2 after its fix rounds: 11 pg_regress tests and 281 TAP
  subtests in 10 files; in tree 30 of 30 at that commit.
- Control plane: Windows and Linux builds, `go vet`, `gofmt -l`, `go test`
  (27 packages), and the WSL integration run with real PostgreSQL (27 test
  binaries, no failure, no skipped test), after each commit.
- Contract 0.4.0: the rule checker passes (69 paths, 86 operations).
- Packaging, built from a clean snapshot of the commit: image test 79
  checks, compose test 58, package test under systemd 158, none failed.
- Panel: typecheck, lint, build and brand check clean; 291 of 292 unit and
  component tests; 17 tests in a headless browser (layout at 1366, 683 and
  320 px, axe in light and dark, and a run of every page under the
  Content-Security-Policy the README prescribes). The one failing test is
  the guard that compares the generated API types with the contract: the
  contract moved to 0.4.0 while the fix round was running, and the panel
  is adapted in the next change.
- The panel's end-to-end run against the real server was run by its author
  only (28 of 28 steps mid-work; a later run stopped at a step that expects
  an operation to be unsupported which the server meanwhile implements).

### Needs-verification items checked

- "The fencing design and the persistence of in-doubt transactions after
  failover come from background knowledge": designed in
  [design/ha-phase5.md](design/ha-phase5.md); not implemented or tested
  yet.
- "Bundling Patroni with a Python runtime; embedded etcd membership": no
  longer applies; see ADR 0021.
- "Node-to-node traffic uses mutual TLS with a cluster CA created at cluster
  init": implemented and tested with three servers.

### Decisions and deviations

- ADR 0021 supersedes ADR 0012 (see above). Automatic failover will be off
  by default, and needs three servers to survive the loss of one.
- A session's roles are re-derived on every request with a cache of three
  seconds; a server that cannot ask its database answers 503 instead of
  trusting the session.
- Any change to a role, including the role changing its own password, ends
  that role's sessions.
- The managed PostgreSQL configuration never logs the text of a failed
  statement, and local settings cannot switch that back on.
- The panel refuses to address objects named `.` or `..`: a browser removes
  such path segments even when they are percent-encoded. The contract needs
  an escape for them or the server must refuse to create them.
- The package's unit restarts the database on `systemctl restart`; a
  restart of the manager alone is `systemctl kill --kill-whom=main
  --signal=SIGTERM kingres`.

### Still open

- **`kingres_shard`**: the second review found eighteen new defects (S3-01
  to S3-18). Two are critical and leave the copies of a reference table
  different without an error: a write made by a function or trigger inside
  a routed statement reaches one copy only, and a cached plan of a write
  keeps writing the local copy after the table becomes a reference table.
  Their fix round has not started. Phase 4 (moving buckets) is being
  implemented; phases 2b and 5 are designed only.
- **Control plane**: 19 of 86 operations still answer "not supported yet"
  (server groups, inclusion proofs, the verification list, instance start
  and stop, leave-cluster, local settings, rebalance, failover). The
  certificate revoke and renew operations and their renewal job, cluster-
  wide gathering of logs, alerts and metrics, and the security headers the
  panel's review asks for are not written.
- **Panel**: not yet adapted to contract 0.4.0; the workbench cannot cancel
  a statement on the server (the contract has no such operation).
- **Packaging**: the compose test does not join the three nodes into one
  cluster yet.
- **CI**: still on a local branch only, for the reason given in the
  previous entry.
- The fix rounds of `kingresd` and of the panel have not had a second
  independent review.

### Core patches

None added in this entry.

---

## 2026-11-14: Sharding 0.2, ledger 0.2, the verifier, packaging, and the control plane up to one real server

### What was done

- **`kingres_shard` 0.2** (phase 2a,
  [design/shard-phase2a.md](design/shard-phase2a.md)): two-phase commit with
  a decision log and a recovery worker, atomic catalog changes, reference
  tables, DDL propagation, a distributed scan for any `SELECT` that spans
  shard groups, cross-group `UPDATE` and `DELETE`, and
  `kingres_shard.node_conninfo`.
- **`kingres_ledger` 0.2** ([design/ledger-0.2.md](design/ledger-0.2.md)):
  updatable ledger tables with a history table, a background worker that
  builds blocks, and digest and verification records. The format
  documentation was completed from the gaps the verifier found, and four
  defects in the server-side proof check were fixed on the way (the format
  member was not checked, the operation accepted any character, a malformed
  later header was ignored, and the user name was hashed in the database
  encoding).
- **`kingres/verifier`**: `kingres-verify`, a standalone verifier written
  from the format documentation alone, standard library only.
- **`kingres/packaging`**: a release build recipe, a container image, a
  three-node compose topology, a Debian package with the unit
  `kingres.service`, and tests of each. A CI workflow exists but is not in
  the repository yet (see "Still open").
- **REST contract 0.3.0** (69 paths, 86 operations).
- **Control plane** ([design/control-plane-2.md](design/control-plane-2.md),
  [design/control-plane-3.md](design/control-plane-3.md)): `kingresd`
  manages the local instance, keeps its state in the system database, runs
  durable tasks, creates a cluster through the API, and implements 64 of
  the 86 operations (databases and tables, roles and grants, network rules,
  the query workbench, ledger status, digests and verification, this
  server, alerts, logs, metrics, cluster settings). The first part of the
  multi-server increment is in: a cluster certificate authority and an
  internal API with mutual TLS.
- **Web panel**: follows contract 0.3.0 and has an end-to-end run against
  the real server (`npm run e2e`).
- **Decisions** recorded as ADRs 0018 (manager leader), 0019 (system
  database) and 0020 (no bundled metrics store).
- **Design** of phase 4 of sharding,
  [design/shard-phase4.md](design/shard-phase4.md).

### How it was verified

- `kingres_shard` 0.2: 11 pg_regress tests and 1850 TAP subtests in 15 files
  pass out of tree, and the whole tree built from the last commit plus this
  version passes both extension suites in tree (29 of 29), run by the
  integrator.
- `kingres_ledger` 0.2: 10 pg_regress tests and 199 TAP subtests in 9 files
  pass out of tree and in tree (21 of 21 at that commit), run by the
  integrator.
- Verifier: `go build`, `go vet`, `gofmt -l` clean and `go test ./...`
  passing, run by the integrator, on fixtures regenerated from a server
  running ledger 0.2; the cross-check passed unchanged, which shows 0.2 did
  not change hash format version 1.
- Packaging: the integrator built the image from a clean snapshot and ran
  its test (23 checks passed). The package test (61 checks: install, service
  start, upgrade, remove, purge in a container with systemd) and the compose
  test (16 checks) were run by the author only.
- Control plane: Windows build, Linux build, `go vet`, `gofmt -l` and
  `go test ./...` clean, and the full WSL integration run (21 packages, no
  skipped test) passing, run by the integrator after each of its three
  commits.
- Contract 0.3.0: Redocly lint and the rule checker pass.
- Panel: typecheck, lint, 180 tests, build and the generated-file checks
  pass, run by the integrator. The end-to-end run (28 steps) was run by the
  author only; the integrator did not repeat it because the control plane
  it builds from was being changed at the time.
- **Two mistakes of the integrator's own tooling, found and fixed.** The
  first in-tree check of sharding 0.2 failed because the check reused a
  build directory and both ninja and the temporary installation decide by
  modification time, so a stale control file was tested; the check now makes
  overlaid files newer and discards the temporary installation. And
  `ext.sh` did not apply an extension's settings file while the in-tree run
  does, so one expected output could not be right in both; `ext.sh` now
  applies it.

### Needs-verification items checked

- "Shard moves use logical replication; does not replicate DDL, sequences,
  TRUNCATE": not checked yet; phase 4 will.
- "2PC recovery by comparing prepared transactions with a local log":
  implemented and tested with fault injection. One rule from the design did
  not survive contact with PostgreSQL: "the transaction id in the gid is not
  running" is not enough after a crash, because a killed backend's
  transaction id can still be reported as in progress; the backend pid is
  compared as well.
- "A hot standby rejects writes; write forwarding would need a core patch":
  not checked; phase 5.

### Decisions and deviations

- Deviations are in the "Implementation notes and deviations" sections of
  the design notes. Those a user notices: an `UPDATE` that assigns the
  distribution column is always refused; multi-row `INSERT`, `INSERT ...
  SELECT` and `COPY FROM` across shard groups are still refused;
  reference-table writes accept only built-in immutable expressions; the
  distributed scan fetches rows only, with no aggregate, sort or limit
  pushdown; the internal API of `kingresd` listens on its own port.
- `kingresd` signs users in and then acts as the signed-in role through the
  local socket with a role map, instead of keeping the sign-in connection
  (which is bound to one database).
- The package installs client programs with a `kingres-` prefix
  (`kingres-psql` and so on) so that they cannot collide with a
  distribution's PostgreSQL packages.

### Still open

- **Reviews in progress or pending**: `kingres_shard` 0.2 (in progress);
  `kingres_ledger` 0.2 was reviewed (eight findings,
  [reviews/2026-11-14-kingres-ledger-0.2.md](reviews/2026-11-14-kingres-ledger-0.2.md))
  and its fix round is in progress; the control plane and the panel have not
  had an independent review.
- **CI**: the workflow file is committed on a local branch only. The access
  token used for pushing may not create workflow files; it needs the
  workflow permission.
- The control plane cannot yet join servers, rebalance or fail over; 22
  operations answer "not supported yet".
- Seven mismatches between the server and the contract found by the panel's
  end-to-end run (for example `GET /ledger/status` answering 404) are being
  fixed.
- Phase 2b of sharding (pushdown for speed), phase 4 (moving buckets) and
  phase 5 (standbys and failover) are not started.
- For a release: signing of the package and image, an apt repository, RPM,
  arm64.

### Core patches

None added in this entry.

---

## 2026-11-14: Reviews, fix rounds, REST contract 0.2 and the first control plane and panel

### What was done

- **Independent reviews** of both extensions, saved in
  [reviews/](reviews/): `2026-11-14-kingres-shard-0.1.md` and
  `2026-11-14-kingres-ledger-0.1.md`.
- **Fix round for `kingres_shard`.** Every finding of the review was fixed,
  each with a test written first: the privilege escalation through the
  `SECURITY DEFINER` event trigger and through the catalog reload (every
  function now pins a `search_path` that ends with `pg_temp`, one function
  keeps definer rights and qualifies everything); session settings leaking
  into forwarded statements (transmission modes around deparsing, forced
  settings on remote sessions); the forwarded-session flag (now
  connection-only, and a forwarded session refuses anything not provably
  local); collation-aware routing; a `ProcessUtility` hook that refuses DDL
  which would touch one node only; a cluster-wide catalog lock and a unique
  primary per group; and statements that reach a distributed table through
  an inlined SQL-language function, which used to return partial results.
- **Fix round for `kingres_ledger`.** All twelve findings were fixed, and the
  hash format was hardened while it can still change freely: a domain tag
  per kind of hashed data, a random chain identity that seeds block 0 and is
  carried by digests and proofs, non-decreasing block times, the
  transaction's user name inside the block transaction leaf, and the table
  bound into proofs. A second protection layer of event triggers enforces
  the rules by OID at the point of action. `ledger_enable` and the tables
  that reveal row hashes are no longer open to PUBLIC.
- **REST contract revision 0.2.0** (`kingres/api/`): 67 paths, 84
  operations, with a rule checker (`scripts/check-contract.mjs`). It fixes
  the defects the two implementers reported against 0.1.0; the changelog is
  in `kingres/api/README.md`.
- **Control plane, first increment** (`kingres/control-plane`, the server
  `kingresd`): the whole contract is routed; 33 of the 84 operations are
  implemented for real (health, setup state and setup token, sign-in and
  sessions, the read side of cluster, servers, shards, databases, tables,
  replication, tasks, roles, grants, settings, ledger and audit), and the
  rest answer with the contract's "not supported yet" problem after
  authentication and authorization have run.
- **Web panel, first increment** (`kingres/web`, Vue 3): every page of the
  first increment against a mock API that is checked against the contract.
  Vue replaced the React originally proposed
  ([ADR 0017](adr/0017-vue-for-the-web-panel.md)).
- **Brand assets** supplied by the project owner are in `kingres/brand/`,
  with usage notes; the README files and the server's placeholder page show
  the logo.
- **Identity.** The developer is Mohammad Rasoul Kazemi (RooyeKhat) and the
  copyright holder is RooyeKhat Media Co; see `kingres/AUTHORS` and
  `kingres/COPYRIGHT`. The Go module path is
  `github.com/KingresDB/Kingres/kingres/control-plane`.
- **History.** The clone holds the full history behind `REL_18_7`, which a
  later rebase needs.

### How it was verified

- **Whole tree:** `build.sh test-all` on the final tree: 338 tests OK, 0
  failed, 23 skipped, 0 timed out. The 23 skips are the same count as on the
  unmodified base; their reasons were not examined.
- **Extensions in tree:** `meson test --suite setup --suite kingres_shard
  --suite kingres_ledger`: 18 of 18 OK (8 pg_regress tests and 7 TAP files
  for sharding; 7 pg_regress tests and 6 TAP files for the ledger).
- **Exploits re-run by the integrator** on a throwaway two-node cluster.
  Before the fix round: an ordinary role became superuser through the
  `sql_drop` event trigger (with one distributed table present), a date
  written under `DateStyle = 'SQL, DMY'` was stored as a different day when
  the statement was forwarded, and an ordinary role could set the
  forwarded-session flag. After it: the role stays unprivileged, the local
  and the forwarded row hold the same date, and setting the flag fails.
- The sharding review file disagreed with the reviewer's first report and
  declared two real findings sound; it carries an integrator's note at its
  top, which is authoritative.
- **A length that depended on the product name.** The ledger's protection
  hook compared trigger names against a prefix with a hand-written length.
  The length was wrong for the final name, and the regression test
  `ledger_protect` caught it (the second protection layer still rejected the
  statement). The length is now derived from the literal.
- **Control plane:** `go build`, `go vet` and `gofmt -l` clean and
  `go test ./...` passing, run by the integrator. The author also reports
  `golangci-lint` with 0 issues, the race detector, and integration tests
  against a real PostgreSQL primary and standby; the integrator did not
  repeat those three.
- **Contract:** Redocly lint reports the file valid and the rule checker
  reports that all rules hold.
- **Panel:** `npm run typecheck`, `npm run lint`, `npm test` (160 tests in 14
  files), `npm run build`, `npm run api:check` and `npm run brand:check`
  passed when run by the integrator on the final tree, with the logos in
  place. The author looked at 17 routes in a headless browser in both themes
  and did one keyboard-only pass; screenshots were examined for about a
  dozen screens only.

### Needs-verification items checked

- "Whether ledger catalogs can be protected even from superusers without a
  core patch": partly. The hook and the event triggers reject the
  statements, including for superusers, but a superuser who edits catalogs
  directly, switches event triggers off, or removes the library from the
  configuration gets past them. The extension's README lists every such
  path. Detection then rests on verification against an external digest.

### Decisions and deviations

- Deviations from the design notes are recorded at the end of each note
  ([design/shard-mvp.md](design/shard-mvp.md),
  [design/ledger-mvp.md](design/ledger-mvp.md)). The ones a user notices:
  on a cluster with more than one shard group, DDL on distributed tables is
  refused until it can be propagated; a comparison under a different
  collation is not used for routing; a statement that reaches a distributed
  table through an inlined SQL-language function is refused; enabling the
  ledger on a table is an administrator action and is permanent.
- **A role named like the extension schema.** With the default
  `search_path` (`"$user", public`), a role named `kingres` resolves
  unqualified names in the schema `kingres` first. The development user in
  WSL has that name, so the tests set `search_path` explicitly. Product
  documentation must warn about it.
- Phase 2 of sharding is split into 2a (correctness on a multi-group
  cluster) and 2b (pushdown for speed);
  [design/shard-phase2a.md](design/shard-phase2a.md).

### Still open

- Six small defects in contract 0.2.0 reported by the implementers (fields
  without a data source that are not nullable, and wording), for revision
  0.3.
- The control plane does not manage instances, run tasks or change anything
  yet; the panel is not connected to it.
- Not reviewed yet: the fixed versions of both extensions (the fix rounds
  were verified by tests and by re-running the exploits, not by a second
  independent review), the control plane, and the panel.
- Known limits of `kingres_shard` 0.1: statements that span shard groups are
  refused; catalog synchronization and cross-node DDL are not atomic;
  user-defined types and non-default collations are not portable between
  nodes; node-to-node authentication is whatever `pg_hba.conf` provides.
- Not supported in `kingres_ledger` 0.1: updatable ledger tables, `pg_dump`
  and restore of a ledger database, partitioned or inherited ledger tables,
  and both extensions on the same table.
- CI, and the mirror branches described in
  [dev/upstream-sync.md](dev/upstream-sync.md).
- A legal review of the name before any public release, including its
  similarity to the name of the Ingres database.

### Core patches

KP-0001 (`meson.build`), KP-0002 (`README.md`), KP-0003
(`contrib/meson.build`).

---

## 2026-11-14: Phase 1 (sharding MVP) and first ledger increment

### What was done

- **`contrib/kingres_shard`, version 0.1** (phase 1), implemented from
  [design/shard-mvp.md](design/shard-mvp.md): the catalog in schema
  `kingres` with a map version, the bucket function, the node registry and
  catalog synchronization, `create_distributed_table`, and statement routing
  through a planner hook and a custom scan, with remote transactions joined
  to the local one. About 4,000 lines of C and a 640-line install script.
- **`contrib/kingres_ledger`, version 0.1** (first increment of phase 6),
  implemented from [design/ledger-mvp.md](design/ledger-mvp.md): append-only
  ledger tables, the capture trigger, RFC 6962 Merkle trees, blocks and
  digests, `ledger_verify`, inclusion proofs that are checked offline, and a
  ProcessUtility hook that rejects DDL which would switch the ledger off.
  About 2,700 lines of C and a 1,000-line install script.
- Both extensions were added to the meson build (core patch KP-0003), and
  `kingres_shard` got a test configuration file so its regression tests run
  in-tree.
- **REST contract.** `kingres/api/openapi.yaml`: OpenAPI 3.1, 65 paths, 82
  operations, with the conventions in `kingres/api/README.md`.
- **Panel framework changed to Vue 3** at the project owner's request
  ([ADR 0017](adr/0017-vue-for-the-web-panel.md)). The few minutes of React
  scaffolding were discarded.

### How it was verified

- Out of tree (PGXS), run by the integrator after the authors:
  `ext.sh kingres_shard all` gives 5 of 5 pg_regress tests and 191 TAP
  subtests in 3 files passing; `ext.sh kingres_ledger all` gives 4 of 4
  pg_regress tests and 45 TAP subtests in 3 files passing.
- In tree: after `build.sh sync configure build`,
  `meson test --suite setup --suite kingres_shard --suite kingres_ledger`
  reports 11 of 11 OK, 0 failed, 0 skipped.
- The full PostgreSQL suite has not been re-run since the extensions were
  added to the build.
- `npx @redocly/cli lint kingres/api/openapi.yaml` reports the file valid.
- Independent code reviews of both extensions were started; their findings
  are not in yet and nothing here should be read as reviewed.

### Needs-verification items checked

- "Partition hash internals (seed, `hash_combine64`)": the extension calls
  PostgreSQL's own `compute_partition_hash_value()` with
  `HASH_PARTITION_SEED`, and a regression test confirms `bucket_for` equals
  the partition a row is really stored in, for int4, int8, text, uuid, date
  and varchar.
- "A hook comparison for capturing writes (trigger, xact callback, logical
  decoding, custom resource manager)": the trigger route was implemented and
  works for the MVP; `ENABLE ALWAYS` triggers do fire under
  `session_replication_role = replica`. The other routes were not tried.
- "ed25519 in pgcrypto", "S3 Object Lock semantics": not checked; signing and
  anchoring are phase 7.

### Decisions and deviations

- Deviations from the two design notes are recorded by the authors in the
  section "Implementation notes and deviations" at the end of each note. The
  ones with consequences for users:
  - A planner hook cannot force a custom plan, so a prepared statement whose
    routing depends on a parameter fails under `plan_cache_mode =
    force_generic_plan` with a hint.
  - The stale-map retry only works when the remote transaction has run no
    earlier statement.
  - Ledger row hashes are computed with the client encoding switched to
    UTF-8, so they do not depend on a session setting.
  - `ledger_enable` rejects composite types, arrays of user-defined types and
    virtual generated columns.
- **Hash format corrected before release.** The design note left the
  transaction's user name outside every hash. It is now part of the block
  transaction leaf, still as format version 1, since no ledger exists yet.
- **A role named `kingres` shadows the schema.** With the default
  `search_path` (`"$user", public`), a role whose name equals the extension
  schema resolves unqualified names in that schema first. The development
  user in WSL is called `kingres`, so the tests set `search_path`
  explicitly. Product documentation must warn about this, or the schema
  should be renamed; not decided.

### Still open

- Review findings for both extensions, and the fixes that follow.
- Three format changes proposed by the ledger author that are free to make
  only before a first release: a domain tag per leaf kind, a chain identity
  so that a digest of one database cannot be checked against another, and a
  rule for `closed_at` (monotonic or documented as advisory).
- Known limits of `kingres_shard` 0.1, all inside the phase 1 scope:
  statements that span shard groups are rejected; catalog synchronization
  and cross-node DDL are not atomic; `COPY`, `TRUNCATE` and later DDL on a
  distributed table are not propagated; user-defined types and non-default
  collations are not portable between nodes.
- Not supported yet in `kingres_ledger` 0.1: `pg_dump` and restore of a
  ledger database, partitioned or inherited ledger tables, and using both
  extensions on the same table.
- The Go control plane and the Vue panel are in progress.

### Core patches

KP-0003 (`contrib/meson.build`).

---

## 2026-11-14: Phase 0, repository, build environment and minimal rebrand

### What was done

- **Repository.** Initialized a git repository in the project directory with
  `core.autocrlf=false` and `core.eol=lf`, added the remote `upstream`
  (https://github.com/postgres/postgres.git), fetched the tag `REL_18_7`
  (commit `f5c41f6`, "Stamp 18.7.") with depth 1, and checked it out as the
  branch `kingres-18`. The clone is shallow; it has to be deepened before the
  first rebase and before pushing to a hosting remote.
- **Layout.** KingresDB's own files live under `kingres/` and
  `contrib/kingres_*`. The design document moved to
  `kingres/docs/architecture.md` and the research notes to
  `kingres/docs/research/`.
- **Minimal rebrand** (core patches KP-0001 and KP-0002, see
  [dev/core-patches.md](dev/core-patches.md)): `meson.build` defines the
  product version 0.1.0, exports `KINGRES_VERSION` and
  `KINGRES_VERSION_NUM`, and appends `(KingresDB 0.1.0)` to
  `PG_VERSION_STR`. The top-level `README.md` gained a KingresDB section.
  Added `kingres/COPYRIGHT`.
- **Build environment.** Installed the WSL2 distribution Ubuntu 24.04 and
  wrote three scripts: `kingres/dev/setup-ubuntu.sh` (toolchain and an
  unprivileged user), `kingres/dev/build.sh` (mirror, configure, build,
  install, test) and `kingres/dev/ext.sh` (build and test one extension with
  PGXS).
- **Developer documentation.** `kingres/README.md`, the documents in
  `kingres/docs/dev/`, and ADRs 0001 to 0016 in `kingres/docs/adr/`.
- **Design notes** for the two extensions:
  [design/shard-mvp.md](design/shard-mvp.md) and
  [design/ledger-mvp.md](design/ledger-mvp.md).

### How it was verified

- `setup-ubuntu.sh` ran to completion as root; gcc 13.3.0, meson 1.3.2,
  ninja 1.11.1, bison 3.8.2, flex 2.6.4 and Perl `IPC::Run` are present.
- `build.sh all`: 1979 build steps completed; `postgres --version` prints
  `postgres (PostgreSQL) 18.7`; the core regression suite passed
  (`regress/regress`: 235 subtests, 4 of 4 meson tests OK).
- `build.sh test-all`: 323 tests OK, 0 failed, 23 skipped, 0 timed out. The
  reasons for the 23 skips were not examined; upstream skips suites that need
  optional libraries or the `PG_TEST_EXTRA` setting.
- `pg_config.h` in the build directory contains
  `KINGRES_VERSION "0.1.0"`, `KINGRES_VERSION_NUM 100` and
  `PG_VERSION_STR "PostgreSQL 18.7 on x86_64-linux, compiled by gcc-13.3.0, 64-bit (KingresDB 0.1.0)"`;
  `PG_VERSION` is still `"18.7"` and `pg_config --version` still prints
  `PostgreSQL 18.7`.
- The installed PGXS tree contains the Perl test modules and `pg_regress`
  that extension tests need.
- Not verified: `ext.sh` has not been run end to end yet; the first extension
  build will exercise it.

### Needs-verification items checked

- "The exact branch names were not confirmed": `git ls-remote` shows both
  `REL_18_STABLE` and `REL_19_STABLE` on the upstream repository.
- "Rebrand checklist, to verify by grep on the real tree": `PG_VERSION_STR` is
  defined in `meson.build` (and in `configure.ac` for autoconf) and used in
  three C files (`version.c`, `postmaster.c`, psql's `startup.c`). Changing
  the one definition covers all three.
- "Current Windows build requirements unconfirmed": not checked. The build
  runs in WSL2, and a native Windows build is not attempted.

### Decisions and deviations

- **Repository layout.** The repository root is the PostgreSQL tree and
  everything else is under `kingres/`, instead of the `postgres/`
  subdirectory in the architecture document's first sketch. Reason: a rebase
  onto upstream then never involves a path prefix. The architecture document
  was updated. (ADR 0003.)
- **Meson only.** The autoconf build files are not patched. (Recorded in
  core-patches.md.)
- **Default bucket count 128 instead of 1024**, because a bucket is stored as
  one hash partition and 1024 partitions per table is too many relations on a
  small cluster. It remains a setting. The project owner should confirm or
  overrule this. (design/shard-mvp.md.)
- **Replica count is per cluster.** The architecture document's database
  dialog and registry table still listed a per-database replica count; both
  were corrected to match decision 6.
- **OpenAPI first.** The architecture document mentioned generating REST from
  gRPC; it now says the hand-written OpenAPI file is the contract. (ADR 0016.)
- **Copyright holder.** `kingres/COPYRIGHT` named a placeholder holder at
  this point; the project owner supplied the real one later the same day
  (see the newest entry).

### Still open

- Each core patch must be its own commit, as
  [dev/core-patches.md](dev/core-patches.md) requires.
- Create the mirror branches described in
  [dev/upstream-sync.md](dev/upstream-sync.md).
- CI: no pipeline exists yet; it depends on the hosting choice.
- A legal review of the name and of the PostgreSQL trademark policy before
  any public release.
- In progress at the time of this entry: the `kingres_shard` and
  `kingres_ledger` extensions and the REST API contract.

### Core patches

KP-0001 (`meson.build`), KP-0002 (`README.md`).
