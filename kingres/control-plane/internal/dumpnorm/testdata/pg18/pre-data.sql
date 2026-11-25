--
-- PostgreSQL database dump
--

\restrict vgRaF9c1MH3d3rxSqSiBGyFvMqW7rOsVafbhObg0NV9maaegUA9MM9q1qUwJhMU

-- Dumped from database version 18.7
-- Dumped by pg_dump version 18.7

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: app; Type: SCHEMA; Schema: -; Owner: -
--

CREATE SCHEMA app;


--
-- Name: kingres_columnar; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS kingres_columnar WITH SCHEMA kingres;


--
-- Name: EXTENSION kingres_columnar; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION kingres_columnar IS 'KingresDB columnar table access method: compressed column-wise storage for analytic scans';


--
-- Name: kingres_ledger; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS kingres_ledger WITH SCHEMA kingres;


--
-- Name: EXTENSION kingres_ledger; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION kingres_ledger IS 'KingresDB tamper-evident ledger: append-only and updatable tables, blocks, digests and proofs';


--
-- Name: kingres_shard; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS kingres_shard WITH SCHEMA pg_catalog;


--
-- Name: EXTENSION kingres_shard; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION kingres_shard IS 'KingresDB sharding: distributed tables, bucket map and statement routing';


--
-- Name: kingres_timepart; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS kingres_timepart WITH SCHEMA kingres;


--
-- Name: EXTENSION kingres_timepart; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION kingres_timepart IS 'KingresDB time partitioning: creation ahead, retention and a run history for ordinary partitioned tables';


--
-- Name: mood; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.mood AS ENUM (
    'sad',
    'ok',
    'happy'
);


--
-- Name: posint; Type: DOMAIN; Schema: app; Owner: -
--

CREATE DOMAIN app.posint AS integer
	CONSTRAINT posint_check CHECK ((VALUE > 0));


SET default_table_access_method = heap;

--
-- Name: items; Type: TABLE; Schema: app; Owner: -
--

CREATE TABLE app.items (
    id bigint NOT NULL,
    label text
);


--
-- Name: items_id_seq; Type: SEQUENCE; Schema: app; Owner: -
--

ALTER TABLE app.items ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME app.items_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: acc; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.acc (
    id bigint NOT NULL,
    bal integer
);


--
-- Name: acc_ledger_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.acc_ledger_history (
    id bigint,
    bal integer,
    ledger_txid bigint,
    ledger_seq integer,
    ledger_op "char"
);


--
-- Name: audit_rows; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_rows (
    id bigint NOT NULL,
    what text
);


SET default_table_access_method = columnar;

--
-- Name: c_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.c_events (
    id bigint,
    v text
);


--
-- Name: d_hash; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash (
    id bigint NOT NULL,
    tenant integer NOT NULL,
    v text
)
PARTITION BY HASH (id);


SET default_table_access_method = heap;

--
-- Name: d_hash_b0000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash_b0000 (
    id bigint CONSTRAINT d_hash_id_not_null NOT NULL,
    tenant integer CONSTRAINT d_hash_tenant_not_null NOT NULL,
    v text
);


--
-- Name: d_hash_b0001; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash_b0001 (
    id bigint CONSTRAINT d_hash_id_not_null NOT NULL,
    tenant integer CONSTRAINT d_hash_tenant_not_null NOT NULL,
    v text
);


--
-- Name: d_hash_b0002; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash_b0002 (
    id bigint CONSTRAINT d_hash_id_not_null NOT NULL,
    tenant integer CONSTRAINT d_hash_tenant_not_null NOT NULL,
    v text
);


--
-- Name: d_hash_b0003; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash_b0003 (
    id bigint CONSTRAINT d_hash_id_not_null NOT NULL,
    tenant integer CONSTRAINT d_hash_tenant_not_null NOT NULL,
    v text
);


--
-- Name: d_hash_b0004; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash_b0004 (
    id bigint CONSTRAINT d_hash_id_not_null NOT NULL,
    tenant integer CONSTRAINT d_hash_tenant_not_null NOT NULL,
    v text
);


--
-- Name: d_hash_b0005; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash_b0005 (
    id bigint CONSTRAINT d_hash_id_not_null NOT NULL,
    tenant integer CONSTRAINT d_hash_tenant_not_null NOT NULL,
    v text
);


--
-- Name: d_hash_b0006; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash_b0006 (
    id bigint CONSTRAINT d_hash_id_not_null NOT NULL,
    tenant integer CONSTRAINT d_hash_tenant_not_null NOT NULL,
    v text
);


--
-- Name: d_hash_b0007; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_hash_b0007 (
    id bigint CONSTRAINT d_hash_id_not_null NOT NULL,
    tenant integer CONSTRAINT d_hash_tenant_not_null NOT NULL,
    v text
);


--
-- Name: d_part; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part (
    id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    v text
)
PARTITION BY HASH (id);


--
-- Name: d_part_b0000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_b0000 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
)
PARTITION BY RANGE (ts);


--
-- Name: d_part_2026_b0000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_2026_b0000 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_b0001; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_b0001 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
)
PARTITION BY RANGE (ts);


--
-- Name: d_part_2026_b0001; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_2026_b0001 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_b0002; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_b0002 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
)
PARTITION BY RANGE (ts);


--
-- Name: d_part_2026_b0002; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_2026_b0002 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_b0003; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_b0003 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
)
PARTITION BY RANGE (ts);


--
-- Name: d_part_2026_b0003; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_2026_b0003 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_b0004; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_b0004 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
)
PARTITION BY RANGE (ts);


--
-- Name: d_part_2026_b0004; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_2026_b0004 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_b0005; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_b0005 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
)
PARTITION BY RANGE (ts);


--
-- Name: d_part_2026_b0005; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_2026_b0005 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_b0006; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_b0006 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
)
PARTITION BY RANGE (ts);


--
-- Name: d_part_2026_b0006; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_2026_b0006 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_b0007; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_b0007 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
)
PARTITION BY RANGE (ts);


--
-- Name: d_part_2026_b0007; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_2026_b0007 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_rest_b0000; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_rest_b0000 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_rest_b0001; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_rest_b0001 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_rest_b0002; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_rest_b0002 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_rest_b0003; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_rest_b0003 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_rest_b0004; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_rest_b0004 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_rest_b0005; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_rest_b0005 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_rest_b0006; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_rest_b0006 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: d_part_rest_b0007; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.d_part_rest_b0007 (
    id bigint CONSTRAINT d_part_id_not_null NOT NULL,
    ts timestamp with time zone CONSTRAINT d_part_ts_not_null NOT NULL,
    v text
);


--
-- Name: ev; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ev (
    id bigint NOT NULL,
    at timestamp with time zone NOT NULL,
    v text
)
PARTITION BY RANGE (at);


--
-- Name: inh_parent; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.inh_parent (
    id integer NOT NULL,
    note text
);


--
-- Name: inh_child; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.inh_child (
    extra integer
)
INHERITS (public.inh_parent);


--
-- Name: lp; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.lp (
    id integer,
    d date,
    v text
)
PARTITION BY RANGE (d);


--
-- Name: lp_2026; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.lp_2026 (
    id integer,
    d date,
    v text
);


--
-- Name: lp_old; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.lp_old (
    id integer,
    d date,
    v text
);


--
-- Name: t_plain; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.t_plain (
    id integer NOT NULL,
    name text DEFAULT 'x'::text NOT NULL,
    mood app.mood,
    qty app.posint,
    tags text[],
    at timestamp with time zone DEFAULT now(),
    n numeric(10,2)
);


--
-- Name: TABLE t_plain; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.t_plain IS 'the plain table';


--
-- Name: COLUMN t_plain.name; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.t_plain.name IS 'the name';


--
-- Name: mv_plain; Type: MATERIALIZED VIEW; Schema: public; Owner: -
--

CREATE MATERIALIZED VIEW public.mv_plain AS
 SELECT count(*) AS c
   FROM public.t_plain
  WITH NO DATA;


--
-- Name: refdata; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.refdata (
    id integer NOT NULL,
    v text
);


--
-- Name: t_child; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.t_child (
    id integer NOT NULL,
    plain_id integer,
    note text,
    gen integer GENERATED ALWAYS AS ((id * 2)) STORED,
    CONSTRAINT t_child_note_check CHECK ((length(note) < 50))
);


--
-- Name: t_plain_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.t_plain_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: t_plain_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.t_plain_id_seq OWNED BY public.t_plain.id;


--
-- Name: v_plain; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.v_plain AS
 SELECT id,
    name
   FROM public.t_plain;


--
-- Name: d_hash_b0000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash ATTACH PARTITION public.d_hash_b0000 FOR VALUES WITH (modulus 8, remainder 0);


--
-- Name: d_hash_b0001; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash ATTACH PARTITION public.d_hash_b0001 FOR VALUES WITH (modulus 8, remainder 1);


--
-- Name: d_hash_b0002; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash ATTACH PARTITION public.d_hash_b0002 FOR VALUES WITH (modulus 8, remainder 2);


--
-- Name: d_hash_b0003; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash ATTACH PARTITION public.d_hash_b0003 FOR VALUES WITH (modulus 8, remainder 3);


--
-- Name: d_hash_b0004; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash ATTACH PARTITION public.d_hash_b0004 FOR VALUES WITH (modulus 8, remainder 4);


--
-- Name: d_hash_b0005; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash ATTACH PARTITION public.d_hash_b0005 FOR VALUES WITH (modulus 8, remainder 5);


--
-- Name: d_hash_b0006; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash ATTACH PARTITION public.d_hash_b0006 FOR VALUES WITH (modulus 8, remainder 6);


--
-- Name: d_hash_b0007; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash ATTACH PARTITION public.d_hash_b0007 FOR VALUES WITH (modulus 8, remainder 7);


--
-- Name: d_part_2026_b0000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0000 ATTACH PARTITION public.d_part_2026_b0000 FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');


--
-- Name: d_part_2026_b0001; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0001 ATTACH PARTITION public.d_part_2026_b0001 FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');


--
-- Name: d_part_2026_b0002; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0002 ATTACH PARTITION public.d_part_2026_b0002 FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');


--
-- Name: d_part_2026_b0003; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0003 ATTACH PARTITION public.d_part_2026_b0003 FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');


--
-- Name: d_part_2026_b0004; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0004 ATTACH PARTITION public.d_part_2026_b0004 FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');


--
-- Name: d_part_2026_b0005; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0005 ATTACH PARTITION public.d_part_2026_b0005 FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');


--
-- Name: d_part_2026_b0006; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0006 ATTACH PARTITION public.d_part_2026_b0006 FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');


--
-- Name: d_part_2026_b0007; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0007 ATTACH PARTITION public.d_part_2026_b0007 FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');


--
-- Name: d_part_b0000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part ATTACH PARTITION public.d_part_b0000 FOR VALUES WITH (modulus 8, remainder 0);


--
-- Name: d_part_b0001; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part ATTACH PARTITION public.d_part_b0001 FOR VALUES WITH (modulus 8, remainder 1);


--
-- Name: d_part_b0002; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part ATTACH PARTITION public.d_part_b0002 FOR VALUES WITH (modulus 8, remainder 2);


--
-- Name: d_part_b0003; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part ATTACH PARTITION public.d_part_b0003 FOR VALUES WITH (modulus 8, remainder 3);


--
-- Name: d_part_b0004; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part ATTACH PARTITION public.d_part_b0004 FOR VALUES WITH (modulus 8, remainder 4);


--
-- Name: d_part_b0005; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part ATTACH PARTITION public.d_part_b0005 FOR VALUES WITH (modulus 8, remainder 5);


--
-- Name: d_part_b0006; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part ATTACH PARTITION public.d_part_b0006 FOR VALUES WITH (modulus 8, remainder 6);


--
-- Name: d_part_b0007; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part ATTACH PARTITION public.d_part_b0007 FOR VALUES WITH (modulus 8, remainder 7);


--
-- Name: d_part_rest_b0000; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0000 ATTACH PARTITION public.d_part_rest_b0000 DEFAULT;


--
-- Name: d_part_rest_b0001; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0001 ATTACH PARTITION public.d_part_rest_b0001 DEFAULT;


--
-- Name: d_part_rest_b0002; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0002 ATTACH PARTITION public.d_part_rest_b0002 DEFAULT;


--
-- Name: d_part_rest_b0003; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0003 ATTACH PARTITION public.d_part_rest_b0003 DEFAULT;


--
-- Name: d_part_rest_b0004; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0004 ATTACH PARTITION public.d_part_rest_b0004 DEFAULT;


--
-- Name: d_part_rest_b0005; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0005 ATTACH PARTITION public.d_part_rest_b0005 DEFAULT;


--
-- Name: d_part_rest_b0006; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0006 ATTACH PARTITION public.d_part_rest_b0006 DEFAULT;


--
-- Name: d_part_rest_b0007; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0007 ATTACH PARTITION public.d_part_rest_b0007 DEFAULT;


--
-- Name: lp_2026; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.lp ATTACH PARTITION public.lp_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');


--
-- Name: lp_old; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.lp ATTACH PARTITION public.lp_old DEFAULT;


--
-- Name: t_plain id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.t_plain ALTER COLUMN id SET DEFAULT nextval('public.t_plain_id_seq'::regclass);


--
-- PostgreSQL database dump complete
--

\unrestrict vgRaF9c1MH3d3rxSqSiBGyFvMqW7rOsVafbhObg0NV9maaegUA9MM9q1qUwJhMU

