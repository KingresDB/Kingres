-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 34: the bundles of the ledger publication for offline verification (phase 7, step 20).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- The file of a bundle lives under the data directory of the server that built it for 24 hours; this is the record that gives it a number.
CREATE TABLE kingres_cp.ledger_bundle (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_name text        NOT NULL,
    server_id     text        NOT NULL,
    created_by    text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    state         text        NOT NULL DEFAULT 'building' CHECK (state IN ('building', 'ready', 'failed')),
    size_bytes    bigint,
    expires_at    timestamptz NOT NULL DEFAULT now() + interval '24 hours'
);
CREATE INDEX ledger_bundle_database ON kingres_cp.ledger_bundle (database_name, id);
