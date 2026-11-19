-- SPDX-License-Identifier: AGPL-3.0-only
-- Portions Copyright (c) 2026, RooyeKhat Media Co
--
-- kingres_columnar 0.1 to 0.2: the stripe catalog gains the state "reserved"
-- (increment C6a).  A stripe row that is reserved is a claim on row numbers that a
-- writer made when its first row went into a buffer, and that the writer replaces
-- with the stripe's row when it writes the stripe.  Every row of a 0.1 catalog is
-- a written stripe, which is what the default says, so nothing is lost and no
-- row is rewritten (a column with a constant default is not stored in the old
-- rows).  Tables written by 0.1 stay readable and writable: their files are
-- format version 2, which this version reads as it reads version 3 (a table
-- reserves row numbers only if its file is of version 3, see the README).

\echo Use "ALTER EXTENSION kingres_columnar UPDATE TO '0.2'" to load this file. \quit

ALTER TABLE columnar_stripe ADD COLUMN reserved boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN columnar_stripe.reserved IS
    'A claim on row numbers, not yet a stripe: a writer that reserves row numbers inserts the row when its first row goes in and replaces it with the stripe row when it writes the stripe. Readers skip it (version 0.2).';
