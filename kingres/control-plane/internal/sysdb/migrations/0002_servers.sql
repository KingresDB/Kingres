-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 2: servers that join or are added (design: control-plane-3.md, D4 and D5).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

ALTER TABLE kingres_cp.server
    ADD COLUMN status             text NOT NULL DEFAULT 'active' CHECK (status IN ('pending', 'active', 'removing')),
    ADD COLUMN internal_address   text,
    ADD COLUMN pg_port            integer,
    ADD COLUMN certificate_serial text,
    ADD COLUMN certificate_pem    text,
    ADD COLUMN public_key_der     bytea,
    ADD COLUMN seen_fingerprint   text,
    ADD COLUMN fingerprint_pinned boolean NOT NULL DEFAULT false,
    ADD COLUMN registering        boolean NOT NULL DEFAULT false;
