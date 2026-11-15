-- SPDX-License-Identifier: PostgreSQL
-- kingres_cp 6: the standbys kingresd manages (high availability, part 1).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- One row per standby instance. kingresd keeps its own record because the sharding catalog
-- cannot hold standbys before kingres_shard 0.4; the catalog registration is one separate
-- step (standby.register_in_catalog) that records its result in catalog_node_id once it exists.
CREATE TABLE kingres_cp.standby (
    standby_id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_id        integer NOT NULL,
    -- the server that runs the standby instance, and the server that ran the group's primary when
    -- the standby was created (the preferred server of the group)
    server_id       text    NOT NULL,
    primary_server  text    NOT NULL,
    -- local instance directory id and port on server_id (chosen free at creation)
    instance_id     integer NOT NULL,
    port            integer,
    slot_name       text    NOT NULL,
    -- creating, ready, rebuilding, removing, failed
    state           text    NOT NULL DEFAULT 'creating'
                    CHECK (state IN ('creating', 'ready', 'rebuilding', 'removing', 'failed')),
    needs_rebuild   boolean NOT NULL DEFAULT false,
    -- counts the rebuilds: a plan with a higher generation clears the "slot lost" mark of the old one
    generation      bigint  NOT NULL DEFAULT 0,
    catalog_node_id integer,
    detail          text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- a server runs at most one standby per group, and a group has one standby per server
    UNIQUE (group_id, server_id)
);

-- The version of the list of standbys that the leader pushes to every server (POST /standby/plan):
-- it grows with every change, and a server never replaces a newer list with an older one, also not
-- after a change of leader (the sequence lives in the system database, which moves with the map group).
CREATE SEQUENCE kingres_cp.standby_plan_version;
