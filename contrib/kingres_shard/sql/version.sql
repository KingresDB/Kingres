-- Core patch KP-0001: the product version is visible next to the PostgreSQL
-- version, without changing any PostgreSQL version symbol.
CREATE EXTENSION kingres_shard;

SELECT version() ~ '^PostgreSQL ' AS starts_with_postgresql;
SELECT version() ~ ' \(KingresDB [0-9]+\.[0-9]+\.[0-9]+\)$' AS ends_with_kingresdb_version;
SELECT kingres.version() ~ '^[0-9]+\.[0-9]+\.[0-9]+$' AS version_has_three_parts;
SELECT kingres.version() =
       substring(version() from '\(KingresDB ([0-9]+\.[0-9]+\.[0-9]+)\)$')
       AS same_version_as_in_version_function;
SELECT current_setting('server_version') !~* 'kingres' AS server_version_has_no_kingres;
SELECT current_setting('server_version') ~ '^18\.' AS server_version_is_postgresql_18;
SELECT current_setting('server_version_num')::int BETWEEN 180000 AND 189999 AS server_version_num_unchanged;

DROP EXTENSION kingres_shard;
