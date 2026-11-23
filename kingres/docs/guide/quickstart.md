<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Quickstart

This page takes you from nothing to a running KingresDB, first one node and then three. It uses the Debian package on Ubuntu 24.04 (amd64). For a container, for the other PostgreSQL major, or for a build from source, see [install.md](install.md).

Choose the major first: `kingres_<version>+pg18-<revision>_amd64.deb` (PostgreSQL 18) or `kingres_<version>+pg19-<revision>_amd64.deb` (PostgreSQL 19). Only one can be installed on a machine, and all servers of a cluster must use the same one.

## One node

1. Install the package. The service starts by itself.

   ```
   sudo apt install ./kingres_0.1.0+pg18-1_amd64.deb
   ```

2. Read the one-time setup token. The node is uninitialized and its console listens on `127.0.0.1:8091` only.

   ```
   sudo cat /var/lib/kingres/setup-token
   ```

3. Open `http://127.0.0.1:8091/` on the server (from another machine use `ssh -L 8091:127.0.0.1:8091 server`), enter the token, choose **Create cluster**, and give the cluster a name and an administrator account. KingresDB creates the database instance, the administrator role, the cluster certificate authority and the system database `kingres_system`, then starts the instance. The token file is removed.

   The same through the API:

   ```
   curl -s -X POST http://127.0.0.1:8091/api/v1/setup/init-cluster \
     -H "X-Setup-Token: $(sudo cat /var/lib/kingres/setup-token)" -H 'Content-Type: application/json' \
     -d '{"clusterName":"demo","admin":{"username":"root_admin","password":"a-long-password"},
          "dataPaths":{"dataDirectory":"/var/lib/kingres"},"memoryQuota":{"totalMegabytes":4096}}'
   curl -s http://127.0.0.1:8091/api/v1/setup/state     # wait until setupTask.state is done
   ```

4. Sign in to the console with the administrator account, and connect with SQL:

   ```
   kingres-psql -h 127.0.0.1 -p 5432 -U root_admin postgres
   ```

   Use TCP, as shown. The administrator logs in with a password; the Unix socket accepts the service user only.

5. Create a database from the console (**Databases**, then create). A database can have sharding on or off, a durability setting and a ledger mode; sharding can be turned on later, never off, and the number of hash buckets (1 to 4096, default 128) is fixed when the database is created.

6. In a database with sharding on, distribute a table (the database page of the console has a dialog for it, or use the API):

   ```
   PUT /api/v1/databases/shop/schemas/public/tables/users/distribution
   {"method":"hash","distributionColumn":"id"}
   ```

   A small table that every node should hold is distributed with `{"method":"reference"}`. On one node this still works, and the data waits for more servers: adding a server and rebalancing moves buckets to it ([operator-guide.md](operator-guide.md)).

A one-node cluster has no standby and no failover. Add servers to get them.

## Three nodes

A cluster is formed by joining servers to the first one. Install the same package on the three machines, then give each machine its own settings **before** the cluster is created or joined.

1. On each machine, set these in `/etc/kingres/kingresd.json` (or as `KINGRES_*` variables in `/etc/kingres/kingresd.env`), using the machine's own name:

   | Setting | Value (example for `db2.example.com`) | Why |
   |---|---|---|
   | `server-host` | `db2.example.com` | The server's id. Its certificate names it, and the other servers connect to it. It must resolve from every other server |
   | `listen` | `0.0.0.0:8091` | The console and REST address (use TLS when it is routable: [install.md](install.md)) |
   | `internal-listen` | `0.0.0.0:8092` | The server-to-server API. It must be **the port above `listen`** |
   | `pg-listen` | `127.0.0.1,db2.example.com` | Where the instance listens; the other servers connect here |
   | `pg-port` | `5432` | The database port |

   Open the internal port (8092) and the database port (5432) between the three machines. The console port is for people and clients only. Restart the service after the change (`sudo systemctl restart kingres`).

2. Create the cluster on the first machine, as in the one-node steps.

3. Add the other two. There are two ways, both produce the same task:

   - **From the cluster** (the console's Servers page, or the API). Sign in on the first server as a `kingres_admin` user, with the setup token of the new machine:

     ```
     curl -s -X POST http://db1.example.com:8091/api/v1/servers -H "Authorization: Bearer $TOKEN" \
       -H 'Content-Type: application/json' \
       -d '{"address":"db2.example.com:8091","setupToken":"<token of db2>"}'
     ```

     The answer is a task (`server.add`); follow it with `GET /api/v1/tasks/{id}` or on the Tasks page. Get `$TOKEN` from `POST /api/v1/auth/login` with `"issueToken": true`. To pin the new server's certificate, send its SHA-256 in `X-Kingres-Pin-Sha256`.
   - **From the new server** (its own setup wizard: **Join cluster**), with its setup token, the address of a member and the administrator of the cluster.

4. Check the cluster on the Dashboard and the Servers page. Each server shows its shard group, its role and its health.

5. With three servers, the cluster can keep standbys and fail over ([operator-guide.md](operator-guide.md), "Failover and switchover"). A session belongs to the server that issued it: sign in on each server you use.

### Three nodes of a development cluster, in containers

If you only want to try it, the repository has a compose file with three nodes of one image. Build the image of one major and start it (needs Docker; see [install.md](install.md) for the image):

```
cd kingres/packaging/docker
./build-image.sh --rev kingres-18             # kingres:<version>-pg18
export KINGRES_IMAGE=kingres:<version>-pg18
docker compose up -d
./tokens.sh                                   # the setup token and the console URL of each node
```

The nodes are `kingres-1` to `kingres-3` with consoles on `127.0.0.1:8091`, `8092` and `8093`. Create the cluster on the first, then add the others as above (the addresses are `kingres-2:8091` and `kingres-3:8091`). The packaging README ([../../packaging/README.md](../../packaging/README.md), "Making the three nodes one cluster") has the details. The compose cluster is for development: its test does not exercise removing a server, failover or rebalance.
