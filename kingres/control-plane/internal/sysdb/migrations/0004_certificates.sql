-- SPDX-License-Identifier: AGPL-3.0-only
-- kingres_cp 4: every server certificate the CA issued, and the revocation of a server's certificates
-- (certificate renewal and revocation, contract 0.5.0).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- A renewal does not revoke the previous certificate of the server (it carries the same key and expires on
-- its own), so revoking "the certificates of a server" must reach every one that is still valid.
CREATE TABLE kingres_cp.certificate (
    serial    text        PRIMARY KEY,
    server_id text        NOT NULL,
    issued_at timestamptz NOT NULL DEFAULT now(),
    -- null: not known (a certificate issued before this table existed); it is then treated as still valid
    not_after timestamptz
);
CREATE INDEX certificate_server ON kingres_cp.certificate (server_id);

INSERT INTO kingres_cp.certificate (serial, server_id, issued_at)
    SELECT certificate_serial, server_id, joined_at FROM kingres_cp.server WHERE certificate_serial IS NOT NULL;

ALTER TABLE kingres_cp.server
    ADD COLUMN certificate_revoked_at timestamptz,
    ADD COLUMN certificate_renewed_at timestamptz;

-- A number for every server that stays with it for as long as it is a member: the ids of the alerts a server keeps
-- are made from it, so that the alerts of all servers have distinct ids and the server that holds an alert is known
-- from its id (cluster-wide alert listing, contract 0.5.0). The identity column numbers the servers that exist.
ALTER TABLE kingres_cp.server
    ADD COLUMN ordinal integer GENERATED ALWAYS AS IDENTITY;
CREATE UNIQUE INDEX server_ordinal ON kingres_cp.server (ordinal);
