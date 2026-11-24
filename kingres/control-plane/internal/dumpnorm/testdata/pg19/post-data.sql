--
-- PostgreSQL database dump
--

\restrict DqfGUqAuI33E1Ak5a33OpuoHce0sdQbtjsrqJXxC1hw9qzg8VSK3MxoA9ggCyee

-- Dumped from database version 19.0
-- Dumped by pg_dump version 19.0

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
-- Name: items items_pkey; Type: CONSTRAINT; Schema: app; Owner: -
--

ALTER TABLE ONLY app.items
    ADD CONSTRAINT items_pkey PRIMARY KEY (id);


--
-- Name: acc acc_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.acc
    ADD CONSTRAINT acc_pkey PRIMARY KEY (id);


--
-- Name: audit_rows audit_rows_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_rows
    ADD CONSTRAINT audit_rows_pkey PRIMARY KEY (id);


--
-- Name: d_hash d_hash_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash
    ADD CONSTRAINT d_hash_pkey PRIMARY KEY (id);


--
-- Name: d_hash_b0000 d_hash_b0000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash_b0000
    ADD CONSTRAINT d_hash_b0000_pkey PRIMARY KEY (id);


--
-- Name: d_hash_b0001 d_hash_b0001_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash_b0001
    ADD CONSTRAINT d_hash_b0001_pkey PRIMARY KEY (id);


--
-- Name: d_hash_b0002 d_hash_b0002_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash_b0002
    ADD CONSTRAINT d_hash_b0002_pkey PRIMARY KEY (id);


--
-- Name: d_hash_b0003 d_hash_b0003_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash_b0003
    ADD CONSTRAINT d_hash_b0003_pkey PRIMARY KEY (id);


--
-- Name: d_hash_b0004 d_hash_b0004_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash_b0004
    ADD CONSTRAINT d_hash_b0004_pkey PRIMARY KEY (id);


--
-- Name: d_hash_b0005 d_hash_b0005_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash_b0005
    ADD CONSTRAINT d_hash_b0005_pkey PRIMARY KEY (id);


--
-- Name: d_hash_b0006 d_hash_b0006_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash_b0006
    ADD CONSTRAINT d_hash_b0006_pkey PRIMARY KEY (id);


--
-- Name: d_hash_b0007 d_hash_b0007_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_hash_b0007
    ADD CONSTRAINT d_hash_b0007_pkey PRIMARY KEY (id);


--
-- Name: d_part d_part_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part
    ADD CONSTRAINT d_part_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_b0000 d_part_b0000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0000
    ADD CONSTRAINT d_part_b0000_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_2026_b0000 d_part_2026_b0000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_2026_b0000
    ADD CONSTRAINT d_part_2026_b0000_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_b0001 d_part_b0001_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0001
    ADD CONSTRAINT d_part_b0001_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_2026_b0001 d_part_2026_b0001_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_2026_b0001
    ADD CONSTRAINT d_part_2026_b0001_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_b0002 d_part_b0002_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0002
    ADD CONSTRAINT d_part_b0002_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_2026_b0002 d_part_2026_b0002_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_2026_b0002
    ADD CONSTRAINT d_part_2026_b0002_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_b0003 d_part_b0003_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0003
    ADD CONSTRAINT d_part_b0003_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_2026_b0003 d_part_2026_b0003_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_2026_b0003
    ADD CONSTRAINT d_part_2026_b0003_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_b0004 d_part_b0004_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0004
    ADD CONSTRAINT d_part_b0004_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_2026_b0004 d_part_2026_b0004_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_2026_b0004
    ADD CONSTRAINT d_part_2026_b0004_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_b0005 d_part_b0005_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0005
    ADD CONSTRAINT d_part_b0005_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_2026_b0005 d_part_2026_b0005_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_2026_b0005
    ADD CONSTRAINT d_part_2026_b0005_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_b0006 d_part_b0006_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0006
    ADD CONSTRAINT d_part_b0006_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_2026_b0006 d_part_2026_b0006_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_2026_b0006
    ADD CONSTRAINT d_part_2026_b0006_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_b0007 d_part_b0007_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_b0007
    ADD CONSTRAINT d_part_b0007_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_2026_b0007 d_part_2026_b0007_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_2026_b0007
    ADD CONSTRAINT d_part_2026_b0007_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_rest_b0000 d_part_rest_b0000_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_rest_b0000
    ADD CONSTRAINT d_part_rest_b0000_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_rest_b0001 d_part_rest_b0001_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_rest_b0001
    ADD CONSTRAINT d_part_rest_b0001_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_rest_b0002 d_part_rest_b0002_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_rest_b0002
    ADD CONSTRAINT d_part_rest_b0002_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_rest_b0003 d_part_rest_b0003_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_rest_b0003
    ADD CONSTRAINT d_part_rest_b0003_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_rest_b0004 d_part_rest_b0004_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_rest_b0004
    ADD CONSTRAINT d_part_rest_b0004_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_rest_b0005 d_part_rest_b0005_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_rest_b0005
    ADD CONSTRAINT d_part_rest_b0005_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_rest_b0006 d_part_rest_b0006_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_rest_b0006
    ADD CONSTRAINT d_part_rest_b0006_pkey PRIMARY KEY (id, ts);


--
-- Name: d_part_rest_b0007 d_part_rest_b0007_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.d_part_rest_b0007
    ADD CONSTRAINT d_part_rest_b0007_pkey PRIMARY KEY (id, ts);


--
-- Name: ev ev_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ev
    ADD CONSTRAINT ev_pkey PRIMARY KEY (id, at);


--
-- Name: inh_parent inh_parent_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.inh_parent
    ADD CONSTRAINT inh_parent_pkey PRIMARY KEY (id);


--
-- Name: refdata refdata_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refdata
    ADD CONSTRAINT refdata_pkey PRIMARY KEY (id);


--
-- Name: t_child t_child_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.t_child
    ADD CONSTRAINT t_child_pkey PRIMARY KEY (id);


--
-- Name: t_plain t_plain_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.t_plain
    ADD CONSTRAINT t_plain_pkey PRIMARY KEY (id);


--
-- Name: d_hash_tenant; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_tenant ON ONLY public.d_hash USING btree (tenant);


--
-- Name: d_hash_b0000_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_b0000_tenant_idx ON public.d_hash_b0000 USING btree (tenant);


--
-- Name: d_hash_b0001_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_b0001_tenant_idx ON public.d_hash_b0001 USING btree (tenant);


--
-- Name: d_hash_b0002_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_b0002_tenant_idx ON public.d_hash_b0002 USING btree (tenant);


--
-- Name: d_hash_b0003_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_b0003_tenant_idx ON public.d_hash_b0003 USING btree (tenant);


--
-- Name: d_hash_b0004_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_b0004_tenant_idx ON public.d_hash_b0004 USING btree (tenant);


--
-- Name: d_hash_b0005_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_b0005_tenant_idx ON public.d_hash_b0005 USING btree (tenant);


--
-- Name: d_hash_b0006_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_b0006_tenant_idx ON public.d_hash_b0006 USING btree (tenant);


--
-- Name: d_hash_b0007_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX d_hash_b0007_tenant_idx ON public.d_hash_b0007 USING btree (tenant);


--
-- Name: t_plain_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX t_plain_name ON public.t_plain USING btree (lower(name));


--
-- Name: d_hash_b0000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_pkey ATTACH PARTITION public.d_hash_b0000_pkey;


--
-- Name: d_hash_b0000_tenant_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_tenant ATTACH PARTITION public.d_hash_b0000_tenant_idx;


--
-- Name: d_hash_b0001_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_pkey ATTACH PARTITION public.d_hash_b0001_pkey;


--
-- Name: d_hash_b0001_tenant_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_tenant ATTACH PARTITION public.d_hash_b0001_tenant_idx;


--
-- Name: d_hash_b0002_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_pkey ATTACH PARTITION public.d_hash_b0002_pkey;


--
-- Name: d_hash_b0002_tenant_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_tenant ATTACH PARTITION public.d_hash_b0002_tenant_idx;


--
-- Name: d_hash_b0003_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_pkey ATTACH PARTITION public.d_hash_b0003_pkey;


--
-- Name: d_hash_b0003_tenant_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_tenant ATTACH PARTITION public.d_hash_b0003_tenant_idx;


--
-- Name: d_hash_b0004_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_pkey ATTACH PARTITION public.d_hash_b0004_pkey;


--
-- Name: d_hash_b0004_tenant_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_tenant ATTACH PARTITION public.d_hash_b0004_tenant_idx;


--
-- Name: d_hash_b0005_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_pkey ATTACH PARTITION public.d_hash_b0005_pkey;


--
-- Name: d_hash_b0005_tenant_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_tenant ATTACH PARTITION public.d_hash_b0005_tenant_idx;


--
-- Name: d_hash_b0006_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_pkey ATTACH PARTITION public.d_hash_b0006_pkey;


--
-- Name: d_hash_b0006_tenant_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_tenant ATTACH PARTITION public.d_hash_b0006_tenant_idx;


--
-- Name: d_hash_b0007_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_pkey ATTACH PARTITION public.d_hash_b0007_pkey;


--
-- Name: d_hash_b0007_tenant_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_hash_tenant ATTACH PARTITION public.d_hash_b0007_tenant_idx;


--
-- Name: d_part_2026_b0000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0000_pkey ATTACH PARTITION public.d_part_2026_b0000_pkey;


--
-- Name: d_part_2026_b0001_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0001_pkey ATTACH PARTITION public.d_part_2026_b0001_pkey;


--
-- Name: d_part_2026_b0002_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0002_pkey ATTACH PARTITION public.d_part_2026_b0002_pkey;


--
-- Name: d_part_2026_b0003_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0003_pkey ATTACH PARTITION public.d_part_2026_b0003_pkey;


--
-- Name: d_part_2026_b0004_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0004_pkey ATTACH PARTITION public.d_part_2026_b0004_pkey;


--
-- Name: d_part_2026_b0005_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0005_pkey ATTACH PARTITION public.d_part_2026_b0005_pkey;


--
-- Name: d_part_2026_b0006_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0006_pkey ATTACH PARTITION public.d_part_2026_b0006_pkey;


--
-- Name: d_part_2026_b0007_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0007_pkey ATTACH PARTITION public.d_part_2026_b0007_pkey;


--
-- Name: d_part_b0000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_pkey ATTACH PARTITION public.d_part_b0000_pkey;


--
-- Name: d_part_b0001_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_pkey ATTACH PARTITION public.d_part_b0001_pkey;


--
-- Name: d_part_b0002_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_pkey ATTACH PARTITION public.d_part_b0002_pkey;


--
-- Name: d_part_b0003_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_pkey ATTACH PARTITION public.d_part_b0003_pkey;


--
-- Name: d_part_b0004_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_pkey ATTACH PARTITION public.d_part_b0004_pkey;


--
-- Name: d_part_b0005_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_pkey ATTACH PARTITION public.d_part_b0005_pkey;


--
-- Name: d_part_b0006_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_pkey ATTACH PARTITION public.d_part_b0006_pkey;


--
-- Name: d_part_b0007_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_pkey ATTACH PARTITION public.d_part_b0007_pkey;


--
-- Name: d_part_rest_b0000_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0000_pkey ATTACH PARTITION public.d_part_rest_b0000_pkey;


--
-- Name: d_part_rest_b0001_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0001_pkey ATTACH PARTITION public.d_part_rest_b0001_pkey;


--
-- Name: d_part_rest_b0002_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0002_pkey ATTACH PARTITION public.d_part_rest_b0002_pkey;


--
-- Name: d_part_rest_b0003_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0003_pkey ATTACH PARTITION public.d_part_rest_b0003_pkey;


--
-- Name: d_part_rest_b0004_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0004_pkey ATTACH PARTITION public.d_part_rest_b0004_pkey;


--
-- Name: d_part_rest_b0005_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0005_pkey ATTACH PARTITION public.d_part_rest_b0005_pkey;


--
-- Name: d_part_rest_b0006_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0006_pkey ATTACH PARTITION public.d_part_rest_b0006_pkey;


--
-- Name: d_part_rest_b0007_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.d_part_b0007_pkey ATTACH PARTITION public.d_part_rest_b0007_pkey;


--
-- Name: acc_ledger_history kingres_ledger_block_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER kingres_ledger_block_insert BEFORE INSERT ON public.acc_ledger_history FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();

ALTER TABLE public.acc_ledger_history ENABLE ALWAYS TRIGGER kingres_ledger_block_insert;


--
-- Name: acc_ledger_history kingres_ledger_block_modify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER kingres_ledger_block_modify BEFORE DELETE OR UPDATE ON public.acc_ledger_history FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();

ALTER TABLE public.acc_ledger_history ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;


--
-- Name: audit_rows kingres_ledger_block_modify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER kingres_ledger_block_modify BEFORE DELETE OR UPDATE ON public.audit_rows FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();

ALTER TABLE public.audit_rows ENABLE ALWAYS TRIGGER kingres_ledger_block_modify;


--
-- Name: acc kingres_ledger_block_truncate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER kingres_ledger_block_truncate BEFORE TRUNCATE ON public.acc FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

ALTER TABLE public.acc ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;


--
-- Name: acc_ledger_history kingres_ledger_block_truncate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER kingres_ledger_block_truncate BEFORE TRUNCATE ON public.acc_ledger_history FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

ALTER TABLE public.acc_ledger_history ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;


--
-- Name: audit_rows kingres_ledger_block_truncate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER kingres_ledger_block_truncate BEFORE TRUNCATE ON public.audit_rows FOR EACH STATEMENT EXECUTE FUNCTION kingres.ledger_block_modify();

ALTER TABLE public.audit_rows ENABLE ALWAYS TRIGGER kingres_ledger_block_truncate;


--
-- Name: acc kingres_ledger_capture; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER kingres_ledger_capture AFTER INSERT OR DELETE OR UPDATE ON public.acc FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture('4');

ALTER TABLE public.acc ENABLE ALWAYS TRIGGER kingres_ledger_capture;


--
-- Name: audit_rows kingres_ledger_capture; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON public.audit_rows FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture('5');

ALTER TABLE public.audit_rows ENABLE ALWAYS TRIGGER kingres_ledger_capture;


--
-- Name: t_child t_child_plain_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.t_child
    ADD CONSTRAINT t_child_plain_id_fkey FOREIGN KEY (plain_id) REFERENCES public.t_plain(id) ON DELETE CASCADE;


--
-- PostgreSQL database dump complete
--

\unrestrict DqfGUqAuI33E1Ak5a33OpuoHce0sdQbtjsrqJXxC1hw9qzg8VSK3MxoA9ggCyee

