-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 33: who registered and who revoked a client key of the ledger (phase 7, step 17).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- The extension records the role that ran the function, which is always the service role (kingresd runs it on behalf of the caller),
-- so the control plane remembers the caller, as it does for a digest (ledger_digest_actor). key_id is the 32 hexadecimal characters
-- of the identifier of the extension.
CREATE TABLE kingres_cp.ledger_client_key_actor (
    database_name text NOT NULL,
    key_id        text NOT NULL,
    registered_by text,
    revoked_by    text,
    PRIMARY KEY (database_name, key_id)
);
