-- kingres_sketch--1.0.sql
--
-- Install script of the kingres_sketch extension, version 1.0: the types
-- hll (HyperLogLog distinct counts), topn (Space-Saving most frequent items)
-- and tdigest (merging t-digest quantiles), their functions and the aggregates
-- that build and union them.  The design is section 9 of
-- kingres/docs/design/analytics-phase12-queries.md; the algorithms, the byte
-- format and the limits are in the README.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Schema: the control file sets "schema = kingres_sketch" and "relocatable =
-- false", so every object below is created in that schema and the extension
-- never shadows a type or function that another extension has in public.
-- The C functions look nothing up by name (the hash function and the output
-- functions are found by OID), so they carry no search_path setting; a
-- setting would cost a GUC save and restore on every row of an aggregate.
-- The regression test "sketch_security" checks that no function of the
-- extension is SECURITY DEFINER and that every object is in the schema.

\echo Use "CREATE EXTENSION kingres_sketch" to load this file. \quit

-- ---------------------------------------------------------------------------
-- hll
-- ---------------------------------------------------------------------------

CREATE TYPE hll;

CREATE FUNCTION hll_in(cstring) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_in'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_out(hll) RETURNS cstring
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_out'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_recv(internal) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_recv'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_send(hll) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_send'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;

CREATE TYPE hll (
	INPUT = hll_in,
	OUTPUT = hll_out,
	RECEIVE = hll_recv,
	SEND = hll_send,
	INTERNALLENGTH = VARIABLE,
	STORAGE = extended
);

CREATE FUNCTION hll_to_bytea(hll) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_to_bytea'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION bytea_to_hll(bytea) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_from_bytea'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE CAST (hll AS bytea) WITH FUNCTION hll_to_bytea(hll);
CREATE CAST (bytea AS hll) WITH FUNCTION bytea_to_hll(bytea);

CREATE FUNCTION hll_empty("precision" int DEFAULT 14) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_empty'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_hash(anyelement) RETURNS bigint
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_hash'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_add(hll, anyelement) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_add'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION hll_add_hash(hll, bigint) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_add_hash'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION hll_union(hll, hll) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_union'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_fold(hll, "precision" int) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_fold'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_cardinality(hll) RETURNS bigint
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_cardinality'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_precision(hll) RETURNS int
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_precision'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_is_empty(hll) RETURNS boolean
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_is_empty'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;

-- State functions of the aggregates.  The state is a pointer ("internal"),
-- so none of these can be called from SQL with a value.
CREATE FUNCTION hll_agg_add(internal, anyelement) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_agg_add'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION hll_agg_add(internal, anyelement, int) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_agg_add_p'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION hll_agg_union(internal, hll) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_agg_union'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION hll_agg_combine(internal, internal) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_agg_combine'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION hll_agg_serial(internal) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_agg_serial'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_agg_deserial(bytea, internal) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_agg_deserial'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION hll_agg_final(internal) RETURNS hll
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_agg_final'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION hll_agg_final_card(internal) RETURNS bigint
	AS 'MODULE_PATHNAME', 'kingres_sketch_hll_agg_final_card'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;

CREATE AGGREGATE hll_add_agg(anyelement) (
	SFUNC = hll_agg_add, STYPE = internal, SSPACE = 12288,
	FINALFUNC = hll_agg_final,
	COMBINEFUNC = hll_agg_combine,
	SERIALFUNC = hll_agg_serial, DESERIALFUNC = hll_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE hll_add_agg(anyelement, "precision" int) (
	SFUNC = hll_agg_add, STYPE = internal, SSPACE = 12288,
	FINALFUNC = hll_agg_final,
	COMBINEFUNC = hll_agg_combine,
	SERIALFUNC = hll_agg_serial, DESERIALFUNC = hll_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE hll_union_agg(hll) (
	SFUNC = hll_agg_union, STYPE = internal, SSPACE = 12288,
	FINALFUNC = hll_agg_final,
	COMBINEFUNC = hll_agg_combine,
	SERIALFUNC = hll_agg_serial, DESERIALFUNC = hll_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE approx_count_distinct(anyelement) (
	SFUNC = hll_agg_add, STYPE = internal, SSPACE = 12288,
	FINALFUNC = hll_agg_final_card,
	COMBINEFUNC = hll_agg_combine,
	SERIALFUNC = hll_agg_serial, DESERIALFUNC = hll_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE approx_count_distinct(anyelement, "precision" int) (
	SFUNC = hll_agg_add, STYPE = internal, SSPACE = 12288,
	FINALFUNC = hll_agg_final_card,
	COMBINEFUNC = hll_agg_combine,
	SERIALFUNC = hll_agg_serial, DESERIALFUNC = hll_agg_deserial,
	PARALLEL = SAFE
);

-- ---------------------------------------------------------------------------
-- topn
-- ---------------------------------------------------------------------------

CREATE TYPE topn;

CREATE FUNCTION topn_in(cstring) RETURNS topn
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_in'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION topn_out(topn) RETURNS cstring
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_out'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION topn_recv(internal) RETURNS topn
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_recv'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION topn_send(topn) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_send'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;

CREATE TYPE topn (
	INPUT = topn_in,
	OUTPUT = topn_out,
	RECEIVE = topn_recv,
	SEND = topn_send,
	INTERNALLENGTH = VARIABLE,
	STORAGE = extended
);

CREATE FUNCTION topn_to_bytea(topn) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_to_bytea'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION bytea_to_topn(bytea) RETURNS topn
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_from_bytea'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE CAST (topn AS bytea) WITH FUNCTION topn_to_bytea(topn);
CREATE CAST (bytea AS topn) WITH FUNCTION bytea_to_topn(bytea);

CREATE FUNCTION topn_empty(capacity int DEFAULT 1000) RETURNS topn
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_empty'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION topn_add(topn, anyelement) RETURNS topn
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_add'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION topn_union(topn, topn) RETURNS topn
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_union'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION topn_list(topn, n int DEFAULT NULL)
	RETURNS TABLE (item text, frequency bigint, max_error bigint)
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_list'
	LANGUAGE C IMMUTABLE PARALLEL SAFE ROWS 100;
CREATE FUNCTION topn_total(topn) RETURNS bigint
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_total'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION topn_capacity(topn) RETURNS int
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_capacity'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;

CREATE FUNCTION topn_agg_add(internal, anyelement) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_agg_add'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION topn_agg_add(internal, anyelement, int) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_agg_add_k'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION topn_agg_union(internal, topn) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_agg_union'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION topn_agg_combine(internal, internal) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_agg_combine'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION topn_agg_serial(internal) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_agg_serial'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION topn_agg_deserial(bytea, internal) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_agg_deserial'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION topn_agg_final(internal) RETURNS topn
	AS 'MODULE_PATHNAME', 'kingres_sketch_topn_agg_final'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;

CREATE AGGREGATE topn_add_agg(anyelement) (
	SFUNC = topn_agg_add, STYPE = internal, SSPACE = 16384,
	FINALFUNC = topn_agg_final,
	COMBINEFUNC = topn_agg_combine,
	SERIALFUNC = topn_agg_serial, DESERIALFUNC = topn_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE topn_add_agg(anyelement, capacity int) (
	SFUNC = topn_agg_add, STYPE = internal, SSPACE = 16384,
	FINALFUNC = topn_agg_final,
	COMBINEFUNC = topn_agg_combine,
	SERIALFUNC = topn_agg_serial, DESERIALFUNC = topn_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE topn_union_agg(topn) (
	SFUNC = topn_agg_union, STYPE = internal, SSPACE = 16384,
	FINALFUNC = topn_agg_final,
	COMBINEFUNC = topn_agg_combine,
	SERIALFUNC = topn_agg_serial, DESERIALFUNC = topn_agg_deserial,
	PARALLEL = SAFE
);

-- ---------------------------------------------------------------------------
-- tdigest
-- ---------------------------------------------------------------------------

CREATE TYPE tdigest;

CREATE FUNCTION tdigest_in(cstring) RETURNS tdigest
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_in'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_out(tdigest) RETURNS cstring
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_out'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_recv(internal) RETURNS tdigest
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_recv'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_send(tdigest) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_send'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;

CREATE TYPE tdigest (
	INPUT = tdigest_in,
	OUTPUT = tdigest_out,
	RECEIVE = tdigest_recv,
	SEND = tdigest_send,
	INTERNALLENGTH = VARIABLE,
	STORAGE = extended
);

CREATE FUNCTION tdigest_to_bytea(tdigest) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_to_bytea'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION bytea_to_tdigest(bytea) RETURNS tdigest
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_from_bytea'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE CAST (tdigest AS bytea) WITH FUNCTION tdigest_to_bytea(tdigest);
CREATE CAST (bytea AS tdigest) WITH FUNCTION bytea_to_tdigest(bytea);

CREATE FUNCTION tdigest_empty(compression int DEFAULT 100) RETURNS tdigest
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_empty'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_add(tdigest, x double precision, weight bigint DEFAULT 1) RETURNS tdigest
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_add'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_union(tdigest, tdigest) RETURNS tdigest
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_union'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_quantile(tdigest, q double precision) RETURNS double precision
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_quantile'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_quantile(tdigest, q double precision[]) RETURNS double precision[]
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_quantiles'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_rank(tdigest, x double precision) RETURNS double precision
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_rank'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_count(tdigest) RETURNS bigint
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_count'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_min(tdigest) RETURNS double precision
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_min'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_max(tdigest) RETURNS double precision
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_max'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_compression(tdigest) RETURNS int
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_compression'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;

CREATE FUNCTION tdigest_agg_add(internal, double precision) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_add'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_add(internal, double precision, int) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_add_c'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_add(internal, double precision, bigint, int) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_add_wc'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_union(internal, tdigest) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_union'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_pct(internal, double precision, double precision) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_pct'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_pct(internal, double precision, double precision, int) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_pct_c'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_combine(internal, internal) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_combine'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_serial(internal) RETURNS bytea
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_serial'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_deserial(bytea, internal) RETURNS internal
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_deserial'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_final(internal) RETURNS tdigest
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_final'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;
CREATE FUNCTION tdigest_agg_final_pct(internal) RETURNS double precision
	AS 'MODULE_PATHNAME', 'kingres_sketch_tdigest_agg_final_pct'
	LANGUAGE C IMMUTABLE PARALLEL SAFE;

CREATE AGGREGATE tdigest_agg(x double precision) (
	SFUNC = tdigest_agg_add, STYPE = internal, SSPACE = 8192,
	FINALFUNC = tdigest_agg_final,
	COMBINEFUNC = tdigest_agg_combine,
	SERIALFUNC = tdigest_agg_serial, DESERIALFUNC = tdigest_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE tdigest_agg(x double precision, compression int) (
	SFUNC = tdigest_agg_add, STYPE = internal, SSPACE = 8192,
	FINALFUNC = tdigest_agg_final,
	COMBINEFUNC = tdigest_agg_combine,
	SERIALFUNC = tdigest_agg_serial, DESERIALFUNC = tdigest_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE tdigest_weighted_agg(x double precision, weight bigint, compression int) (
	SFUNC = tdigest_agg_add, STYPE = internal, SSPACE = 8192,
	FINALFUNC = tdigest_agg_final,
	COMBINEFUNC = tdigest_agg_combine,
	SERIALFUNC = tdigest_agg_serial, DESERIALFUNC = tdigest_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE tdigest_union_agg(tdigest) (
	SFUNC = tdigest_agg_union, STYPE = internal, SSPACE = 8192,
	FINALFUNC = tdigest_agg_final,
	COMBINEFUNC = tdigest_agg_combine,
	SERIALFUNC = tdigest_agg_serial, DESERIALFUNC = tdigest_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE approx_percentile(q double precision, x double precision) (
	SFUNC = tdigest_agg_pct, STYPE = internal, SSPACE = 8192,
	FINALFUNC = tdigest_agg_final_pct,
	COMBINEFUNC = tdigest_agg_combine,
	SERIALFUNC = tdigest_agg_serial, DESERIALFUNC = tdigest_agg_deserial,
	PARALLEL = SAFE
);
CREATE AGGREGATE approx_percentile(q double precision, x double precision, compression int) (
	SFUNC = tdigest_agg_pct, STYPE = internal, SSPACE = 8192,
	FINALFUNC = tdigest_agg_final_pct,
	COMBINEFUNC = tdigest_agg_combine,
	SERIALFUNC = tdigest_agg_serial, DESERIALFUNC = tdigest_agg_deserial,
	PARALLEL = SAFE
);

-- ---------------------------------------------------------------------------
-- self_test
-- ---------------------------------------------------------------------------

CREATE FUNCTION self_test() RETURNS text
	AS 'MODULE_PATHNAME', 'kingres_sketch_self_test'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;

COMMENT ON TYPE hll IS 'HyperLogLog sketch for approximate distinct counts';
COMMENT ON TYPE topn IS 'Space-Saving summary of the most frequent items';
COMMENT ON TYPE tdigest IS 'merging t-digest for approximate quantiles';
COMMENT ON FUNCTION hll_hash(anyelement) IS '64-bit hash of an element, the value hll_add adds';
COMMENT ON FUNCTION hll_union(hll, hll) IS 'union of two sketches of the same precision; idempotent';
COMMENT ON FUNCTION hll_fold(hll, int) IS 'reduce a sketch to a lower precision';
COMMENT ON FUNCTION hll_cardinality(hll) IS 'estimated number of distinct elements';
COMMENT ON FUNCTION topn_union(topn, topn) IS 'merge of two summaries of the same capacity; frequencies add';
COMMENT ON FUNCTION topn_list(topn, int) IS 'the counters of a summary by frequency descending: item, count (upper bound), error';
COMMENT ON FUNCTION tdigest_union(tdigest, tdigest) IS 'merge of two digests of the same compression; weights add';
COMMENT ON FUNCTION tdigest_quantile(tdigest, double precision) IS 'estimated value at a fraction between 0 and 1';
COMMENT ON FUNCTION tdigest_rank(tdigest, double precision) IS 'estimated fraction of the weight at or below a value';
COMMENT ON FUNCTION self_test() IS 'one line that must be equal on every node that exchanges sketches';
COMMENT ON AGGREGATE hll_add_agg(anyelement) IS 'HyperLogLog sketch of the non-null inputs, precision 14';
COMMENT ON AGGREGATE hll_union_agg(hll) IS 'union of sketches';
COMMENT ON AGGREGATE approx_count_distinct(anyelement) IS 'approximate number of distinct non-null inputs, precision 14';
COMMENT ON AGGREGATE topn_add_agg(anyelement) IS 'Space-Saving summary of the non-null inputs, capacity 1000';
COMMENT ON AGGREGATE topn_union_agg(topn) IS 'merge of summaries';
COMMENT ON AGGREGATE tdigest_agg(double precision) IS 't-digest of the non-null inputs, compression 100';
COMMENT ON AGGREGATE tdigest_union_agg(tdigest) IS 'merge of digests';
COMMENT ON AGGREGATE approx_percentile(double precision, double precision) IS 'approximate percentile: the fraction first, then the value';

CREATE FUNCTION sketch_kind(bytea) RETURNS text
	AS 'MODULE_PATHNAME', 'kingres_sketch_sketch_kind'
	LANGUAGE C IMMUTABLE STRICT PARALLEL SAFE;
COMMENT ON FUNCTION sketch_kind(bytea) IS 'kind of a valid sketch value or aggregate state; an error for bytes that are not one';

-- The bytes held now by the aggregate states of the calling backend: the
-- counter that the limit bounds (the smaller of the settings
-- kingres_sketch.state_memory_ceiling and kingres_sketch.state_memory_limit).  A
-- parallel worker counts for itself, so the function is parallel restricted
-- (it runs in the leader, whose count is the one of the session).
CREATE FUNCTION state_bytes() RETURNS bigint
	AS 'MODULE_PATHNAME', 'kingres_sketch_state_bytes'
	LANGUAGE C VOLATILE STRICT PARALLEL RESTRICTED;
COMMENT ON FUNCTION state_bytes() IS 'bytes held by the aggregate states of this backend now; bounded by state_limit()';

-- The limit in force in the calling backend, in bytes: the smaller of the
-- ceiling that only a superuser sets and the role's own limit.  Parallel
-- restricted for the same reason as state_bytes().
CREATE FUNCTION state_limit() RETURNS bigint
	AS 'MODULE_PATHNAME', 'kingres_sketch_state_limit'
	LANGUAGE C VOLATILE STRICT PARALLEL RESTRICTED;
COMMENT ON FUNCTION state_limit() IS 'the limit in force for the aggregate states of this backend, in bytes: the smaller of kingres_sketch.state_memory_ceiling and kingres_sketch.state_memory_limit';
