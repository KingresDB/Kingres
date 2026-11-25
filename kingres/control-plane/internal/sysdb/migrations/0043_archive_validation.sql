-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (c) 2026, RooyeKhat Media Co
-- kingres_cp 43: the purpose of an upload and the state of the validation of a database archive (phase 14b).
-- Applied inside one transaction by internal/sysdb; do not add BEGIN/COMMIT.

-- What an upload is for: a CSV file for table import, or a zip archive made by the database export. The archive report of a validation
-- is stored in the report column of the row, with the SHA-256 of the file that was read; archive_state says where the validation is.
ALTER TABLE kingres_cp.data_transfer
    ADD COLUMN purpose text CHECK (purpose IS NULL OR purpose IN ('tableImport', 'databaseArchive')),
    ADD COLUMN archive_state text CHECK (archive_state IS NULL OR archive_state IN ('validating', 'valid', 'invalid'));

UPDATE kingres_cp.data_transfer SET purpose = 'tableImport' WHERE kind = 'upload';

-- An archive validation is looked up by the SHA-256 of the file it read (the cache of the validation).
CREATE INDEX data_transfer_archive ON kingres_cp.data_transfer (sha256) WHERE archive_state IS NOT NULL;
