<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Upgrading

Three things change when you upgrade: the package (the programs, including `kingresd` and the console), the extensions in each database (SQL objects), and the cluster's own state in the system database. The package does the first and the third by itself. The second you do by hand.

Before you begin: back up ([operator-guide.md](operator-guide.md), section 7), and read the release notes and the [CHANGELOG](../../CHANGELOG.md) for the version. This is the first release, so no earlier release exists to upgrade from; the paths below are the ones the code and the tests support for the steps between its versions.

## What cannot be done

- **Between PostgreSQL majors.** There is no procedure to move a cluster from 18 to 19 or back. The package refuses it: its `preinst` script stops before anything is unpacked when the other major is installed (or removed without purge), or when a data directory of the other major exists. The sharding extension refuses a node of another major. To go from 18 to 19, build a new cluster of 19 servers and move the data (the extension README, "Requirements and installation"). Do not leave nodes of two majors running side by side except for the short time of an upgrade of the whole cluster that has stopped the traffic. A rolling in-place upgrade and a standby of another major (impossible with physical replication) are not supported.
- **Downgrade and rollback.** There is no rollback of a package upgrade or of an `ALTER EXTENSION ... UPDATE`. Restore from a backup.

## The package

```
sudo apt install ./kingres_<new version>+pg<same major>-<revision>_amd64.deb
```

- Your changes to `/etc/kingres/kingresd.json` and `kingresd.env` are kept (conffiles). Data in `/var/lib/kingres` is untouched.
- The upgrade restarts the service, and with it the database: new server programs must not run under old processes. Upgrade one server at a time, and wait for it to come back (Servers page) before the next. A server of the new version next to servers of the old one raises the alert `versionMismatch` until the last one is upgraded; treat that as the progress of an upgrade, and finish it before you do anything else (no rebalance, no failover drill, no change of tables). In particular make no planned switchover and no restore of preferred primaries while the versions are mixed: a server of the older version does not honour the request to read the proof of a stop alone, and a repeated read can then release a lease that the switchover just took; the leader logs the warning "failover: the server of the old primary did not honour the request for the proof alone" when it happens; search the log for it.
- A package upgrade was tested for a revision bump of the same version (`+pg18-1` to `+pg18-2`). Upgrades between different KingresDB versions are exercised by the extension tests and the cluster tests, not by the package tests.
- Nothing in the package updates the extensions of your databases. Do that next.

## The extensions: `ALTER EXTENSION ... UPDATE`

A database keeps the SQL objects of the version it was created with. After the new library is loaded, update each database that uses the extension, on **every server**, with a superuser. The paths that exist:

| Extension | Default version of this release | Update paths | Notes |
|---|---|---|---|
| `kingres_shard` | 0.9 | 0.1 to 0.2 to 0.3 to 0.4 to 0.5 to 0.6 to 0.7 to 0.8 to 0.9, one `ALTER EXTENSION kingres_shard UPDATE` takes any of them to 0.9 | See the order below |
| `kingres_ledger` | 0.3 | 0.1 to 0.2, 0.2 to 0.3, and 0.1 to 0.3 in one step | See the note below |
| `kingres_columnar` | 0.2 | 0.1 to 0.2 | Update it before `kingres_timepart` converts an indexed partition |
| `kingres_timepart` | 0.3 | 0.1 to 0.2, 0.2 to 0.3; one `ALTER EXTENSION kingres_timepart UPDATE` takes any of them to 0.3 | 0.2 adds the conversion of old partitions to columnar storage; 0.3 adds policies on user-partitioned distributed tables, which need `kingres_shard` 0.9 |
| `kingres_sketch` | 1.0 | none (one version) | |

`kingres_shard` 0.8 is phases 10 and 11 (splitting a bucket, weights, plans by disk size, distributed schemas); 0.9 adds columnar tables, the refusal of ledger tables by `create_distributed_table`, approximate `count(DISTINCT)`, and schemas in the rebalance plan and in `check_cluster`. Repartitioned joins and `MERGE` on distributed tables add no SQL object. The version check of a bucket move asks for 0.9 on every active primary.

```
ALTER EXTENSION kingres_shard UPDATE;      -- in each database, on each server
```

**`kingres_shard`: the order matters.** A node whose library is new and whose SQL objects are not yet updated cannot answer the helper functions that the other nodes call; the library says so ("runs an older version of the kingres_shard SQL objects"). Distributed scans and fan-out writes of the whole cluster fail from the moment a node restarts with the new library until every node has run `ALTER EXTENSION kingres_shard UPDATE`; single-key reads and writes keep working. So:

1. Install the new package on every server, one after the other (each restarts).
2. Run `ALTER EXTENSION kingres_shard UPDATE` on every node, at once and soon after.
3. Do not start a bucket move before that is done: a move refuses unless every active primary reports the current extension version, and names the node that does not.

Until a node has been updated, `create_distributed_table` and `create_reference_table` refuse with "the kingres_shard objects of this database are older than the library". A statement that was pushed down can fail with `KS001` while versions differ; the client repeats it after the upgrade has reached every node. Nodes of the previous protocol and of the new one do not cooperate on the fragments that phase 2b sends: while the cluster is mixed, set `kingres_shard.enable_pushdown`, `enable_bulk_routing` and `enable_copy_routing` to `off`, and set them back to `on` when the last node is upgraded. The extension README ("Requirements and installation") has the full text.

**`kingres_ledger`.** After the library is new and before `ALTER EXTENSION kingres_ledger UPDATE` has run, the capture trigger keeps working and your applications keep working, but the block builder does not, and the ledger is not written until the update has run. Update soon. The console can do it for you: the Ledger page, or `POST /api/v1/databases/{db}/ledger/upgrade` (task `ledger.upgrade`, every server). Client keys and signature policies need 0.3 in every group.

**`kingres_columnar` and `kingres_timepart`.** Install the new library, then `ALTER EXTENSION kingres_columnar UPDATE`. `kingresd` preloads `kingres_columnar` (an instance made by an earlier package gets the new list when `kingresd` starts), but the list takes effect only at a restart of each instance, standbys of a group before its primary; a conversion to columnar storage is refused until every instance has loaded the library ([limitations.md](limitations.md)).

## `kingresd` and the cluster state

- The system database `kingres_system` is migrated by `kingresd` itself: its migrations run at every start and after a join, and a migration that was skipped is applied late. You do nothing.
- The contract (REST API) is additive between revisions. A client written for an older revision keeps working; the console and `kingresd` of one package belong together.
- Settings: a new version may add settings; unknown keys in `kingresd.json` stop the server at start, so remove a key that a newer or older version does not know.
- The failover journal and the tasks survive the restart of an upgrade; a task that was running is resumed by the leader after it restarts.

## An order for a whole cluster

1. Back up.
2. Upgrade one server at a time and wait for each to be healthy. A suggested order, which is a recommendation and not a tested procedure: servers that hold only standbys first, the manager leader last.
3. When every server runs the new package, update the extensions in every database on every server (the order above), and check the alert list: `versionMismatch` must be gone.
4. Run `SELECT * FROM kingres.check_cluster()` on the map group primary and look at the Dashboard.
5. Only then resume rebalance and table changes.
