-- ledger_sign: client signatures (0.3, part B, src/sign.c): the registry of
-- keys, ledger_attest with every error (KL010 to KL014), the policy
-- "required", the three tables as ledger tables (guard, protection, entries,
-- proofs) and what ledger_verify says about them, in a database of its own.
--
-- The signatures are fixtures: the identity of the chain is fixed below (the
-- test replaces it, with the triggers bypassed as ledger_verify does, before
-- any block exists), so a signature made once is valid in every run.  They were
-- made with the openssl command, with keys that were thrown away, over the
-- message of the design (kingres/docs/design/ledger-phase7.md, 3.7):
--
--   openssl genpkey -algorithm ed25519 -out k.pem
--   message = "kingres-ledger/client-signature/v1" 0x00 chain_id table_id(u32)
--             op row_hash key_id nonce
--   openssl pkeyutl -sign -inkey k.pem -rawin -in message -out signature
--
-- The row hashes are pinned below (a change of the hash format fails the
-- first assertion, not the signatures one by one).  Table ids: the three
-- tables of client signatures are 1 to 3, docs is 4 and items is 5.  The
-- script t/032_attest_policy.pl signs with fresh keys and a real chain.
--
-- Portions Copyright (c) 2026, RooyeKhat Media Co
-- SPDX-License-Identifier: AGPL-3.0-only
SET search_path = public;
\set VERBOSITY terse
\set chain_id '0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20'
\set pub_k1 'b1615c9a224b7bdfcb4bc1c0e7b7336d2dd6322cd2a075916e518141ea27c8a9'
\set pub_k2 '1cc357d5b13e9783cf9a1c07d845d7dd11be37824ce87926eba6ce98d47a2ebf'
\set pub_k3 '013530e2e9bb2bac5dd53b23cdfd9faf8ae1dea2481e2b25de414aa6c1a35287'
\set h_r1_k1 'ecc5a7b5f327ecdf6e253755188e1aa83461fc5c41baec802b6b4524c0ff270a'
\set n_r1_k1 '01010101010101010101010101010101'
\set s_r1_k1 'fcf10ad57e43318e7400dfce15129e8dffb6a6626a47586359425c4d11dabc63ddc8b41dbe1f05e8070e43ee1c44a2f57084db2026c58ff5e30696b6f5ca3709'
\set h_r2_k1 '4eb2b9153bdc9de20782af966d123a7cc2a43849cc40294d69ec89f696760f86'
\set n_r2_k1 '02020202020202020202020202020202'
\set s_r2_k1 'ca7ecd4f53b1a28993676dc5b5e77d5c4a83aa5d5590bdbee38eeccef41cd747459b5a553374276d879556a5a73a5e3cc0e4b42d09ecd1b36c68b720f982ca0b'
\set h_r3_k1_replay '74561c03e3d101eb59fd700752c688c0353a45803f1d4e371868a4a1d563b706'
\set n_r3_k1_replay '01010101010101010101010101010101'
\set s_r3_k1_replay '18a37a4a6365b7ed121825a2ca5073769e8505fa2f7590cda2803a04eff7579d668217ebfce7c645006ae234741a815976b3f1cb9eee27b70f6ec7f922a0b008'
\set h_r4_k2 '3832a1a453b8324f8f938433c80dc2c47c89b3f11cfbd9cae570391c5700c322'
\set n_r4_k2 '03030303030303030303030303030303'
\set s_r4_k2 '1e38b9bcec8a5f69e53d82657cacc62bd1c82b52a1fd656f186082939bbe95c6228e6749edc6f33ca20090ec8b2f7338be2c7db476825407362b0ce8a32bac06'
\set h_r5_k1_other_chain 'a38f683d04717aa0a272ac753a283dc21ce8ba57462a62668e4a7804d1cd7622'
\set n_r5_k1_other_chain '04040404040404040404040404040404'
\set s_r5_k1_other_chain 'cabb29b9c73d10cb686f05a96f413e71dff6b38c1e2134ad4c8e087faaba7025de06bae132a541b6220cd03d8c448311a3a8c09582c70f1631afe9dbe0c2f90f'
\set h_u_old_k1 'b52d09af3681b90c9d04c0bcbea4f4fd53efd4b66c3b7a98262160b5a4690e10'
\set n_u_old_k1 '05050505050505050505050505050505'
\set s_u_old_k1 '65553fadb0da758d88610404d6691881c6902ab3e0c0b0844c72da69885016699593900657b792aa3035f4c5cb603bc2f26d8e1ea106bab5d0a0431c06255308'
\set h_u_new_k1 'b2b64930724cbfd6ac9c1af0b2435fe88cb10915caedd400adb69d72f61ddda9'
\set n_u_new_k1 '06060606060606060606060606060606'
\set s_u_new_k1 '5d4ca61d5fb0ddc9a190e8bf38389ae4c1b6277a5710cbbdf60d56bb8c1145563fe853c80c6f5161321802e08120d1aaa2619f8da1d2c14500f38c5e38449502'
\set h_r6_k3 '22e64ce81000eec9310185af22164d59e16c4238a159a764059dc0b266c66002'
\set n_r6_k3 '07070707070707070707070707070707'
\set s_r6_k3 'f29269013dd63bdaccb68ec219584723696a665daae8bf4087ceebb17c2afd9dec062835df2fb2a78b9672133e7142b986f0b1d4645ed4c35fe667e9cfb52804'
\set h_r1_k1_as_d 'ecc5a7b5f327ecdf6e253755188e1aa83461fc5c41baec802b6b4524c0ff270a'
\set n_r1_k1_as_d '08080808080808080808080808080808'
\set s_r1_k1_as_d '970aba8a200b98593d446bc9178d8562344a7d83e7d89f29c364f3955f4192732f6de135b352b4f19d54230f826261e7368056a773280e9a0bbfc5d2e192b909'
\set h_r7_k1 '524f19d8522b756268c649fc011be846e91ab2efba6677930e862158623d9198'
\set n_r7_k1 '09090909090909090909090909090909'
\set s_r7_k1 '26fca563f99e829612e05bb4c0a9cc1de46429f2a84e758196722488ed3927f0516fb4f1c42640aab3a732bef184b3857be6544fb7834acaf685eb9cf3150609'
\set h_r8_k1 '663de600e96554ef2d15972082740215db7a6a5863c0ab5084042b675cc8e70e'
\set n_r8_k1 '0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a'
\set s_r8_k1 'c81bbb356df2c06736915db0ca249899f10e459e2a0b1faff7aaf79efb6482d044f2a7be1c3b2ebad023abf27c86c09b927af1a6c1b19b94d58a2eb46603f00c'
\set h_r8_k1_c '663de600e96554ef2d15972082740215db7a6a5863c0ab5084042b675cc8e70e'
\set n_r8_k1_c '0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b'
\set s_r8_k1_c 'd0b0dda922ea662db67c7ed16aacf6dc910aedd0eedc677e230bf7726c09bf63790fa193dea87247e9cda1914b2f6b1ba45528520320b89a6fe5dcebf89c8703'

SELECT current_database() AS orig_db \gset
DROP DATABASE IF EXISTS kl_sign_t;
CREATE DATABASE kl_sign_t;
\c kl_sign_t
SET search_path = public;
CREATE EXTENSION kingres_ledger;

-- The identity of the chain is fixed (nothing is chained to it yet).
UPDATE pg_trigger SET tgenabled = 'D'
 WHERE tgrelid = 'kingres.ledger_chain'::regclass AND tgname = 'kingres_ledger_block_modify';
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_chain SET chain_id = decode(:'chain_id', 'hex');
UPDATE pg_trigger SET tgenabled = 'A'
 WHERE tgrelid = 'kingres.ledger_chain'::regclass AND tgname = 'kingres_ledger_block_modify';
\c
SET search_path = public;
\set VERBOSITY terse
SELECT encode(chain_id, 'hex') = :'chain_id' AS chain_fixed FROM kingres.ledger_chain;

--------------------------------------------------------------------------
-- The tables of client signatures are registered ledger tables
--------------------------------------------------------------------------
GRANT USAGE ON SCHEMA kingres TO PUBLIC;
SELECT table_id, relid, mode, signature_policy, signature_required_from IS NULL AS no_switch
  FROM kingres.ledger_table ORDER BY table_id;
SELECT a.attnum, a.attname FROM pg_attribute a
 WHERE a.attrelid = 'kingres.ledger_table'::regclass AND a.attnum > 0 AND NOT a.attisdropped
 ORDER BY a.attnum;
SELECT count(*) AS findings FROM kingres.ledger_verify();

CREATE ROLE kl_sign_app;
CREATE ROLE kl_sign_other;
CREATE ROLE kl_sign_wad IN ROLE pg_write_all_data;
GRANT EXECUTE ON FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea),
    kingres.ledger_set_signature_policy(regclass, text),
    kingres.ledger_client_key_register(bytea, name, text),
    kingres.ledger_client_key_revoke(bytea, text) TO kl_sign_app, kl_sign_other, kl_sign_wad;
CREATE TABLE docs (id int PRIMARY KEY, body text);
CREATE TABLE items (id int PRIMARY KEY, body text);
ALTER TABLE docs OWNER TO kl_sign_app;
ALTER TABLE items OWNER TO kl_sign_app;
GRANT INSERT ON docs TO kl_sign_other;
SET client_min_messages = error;
SELECT kingres.ledger_enable('docs');
SELECT kingres.ledger_enable('items', 'updatable');
RESET client_min_messages;
SELECT table_id, relid FROM kingres.ledger_table WHERE table_id > 3 ORDER BY table_id;

-- The row hashes of the fixtures.
SELECT encode(kingres.ledger_row_hash(ROW(1, 'one')::docs), 'hex') = :'h_r1_k1' AS r1_pinned,
       encode(kingres.ledger_row_hash(ROW(2, 'two')::docs), 'hex') = :'h_r2_k1' AS r2_pinned,
       encode(kingres.ledger_row_hash(ROW(4, 'four')::docs), 'hex') = :'h_r4_k2' AS r4_pinned,
       encode(kingres.ledger_row_hash(ROW(1, 'x')::items), 'hex') = :'h_u_old_k1' AS item_old_pinned,
       encode(kingres.ledger_row_hash(ROW(1, 'y')::items), 'hex') = :'h_u_new_k1' AS item_new_pinned;

--------------------------------------------------------------------------
-- The message
--------------------------------------------------------------------------
SELECT encode(kingres.ledger_attestation_message(decode(:'chain_id', 'hex'), 4, 'I',
           decode(:'h_r1_k1', 'hex'), decode(repeat('ab', 16), 'hex'), decode(:'n_r1_k1', 'hex')), 'hex') AS message;
SELECT length(kingres.ledger_attestation_message(decode(:'chain_id', 'hex'), 4, 'D',
           decode(:'h_r1_k1', 'hex'), decode(repeat('ab', 16), 'hex'), decode(:'n_r1_k1', 'hex'))) AS message_length;
\set VERBOSITY sqlstate
SELECT kingres.ledger_attestation_message(decode('00', 'hex'), 4, 'I', decode(:'h_r1_k1', 'hex'), decode(repeat('ab', 16), 'hex'), decode(:'n_r1_k1', 'hex'));
SELECT kingres.ledger_attestation_message(decode(:'chain_id', 'hex'), 4, 'X', decode(:'h_r1_k1', 'hex'), decode(repeat('ab', 16), 'hex'), decode(:'n_r1_k1', 'hex'));
SELECT kingres.ledger_attestation_message(decode(:'chain_id', 'hex'), -1, 'I', decode(:'h_r1_k1', 'hex'), decode(repeat('ab', 16), 'hex'), decode(:'n_r1_k1', 'hex'));
\set VERBOSITY terse
SELECT has_function_privilege('public', 'kingres.ledger_attestation_message(bytea, int, "char", bytea, bytea, bytea)', 'execute') AS message_for_public,
       has_function_privilege('public', 'kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea)', 'execute') AS attest_for_public,
       has_function_privilege('public', 'kingres.ledger_client_key_register(bytea, name, text)', 'execute') AS register_for_public,
       has_function_privilege('public', 'kingres.ledger_client_key_revoke(bytea, text)', 'execute') AS revoke_for_public,
       has_function_privilege('public', 'kingres.ledger_set_signature_policy(regclass, text)', 'execute') AS policy_for_public;

--------------------------------------------------------------------------
-- Keys
--------------------------------------------------------------------------
SELECT encode(kingres.ledger_client_key_register(decode(:'pub_k1', 'hex'), NULL, 'key one'), 'hex') AS key_id \gset k1_
SELECT encode(kingres.ledger_client_key_register(decode(:'pub_k2', 'hex'), 'kl_sign_app', 'key two'), 'hex') AS key_id \gset k2_
SELECT encode(kingres.ledger_client_key_register(decode(:'pub_k3', 'hex'), NULL, 'key three'), 'hex') AS key_id \gset k3_
SELECT :'k1_key_id' = encode(substr(sha256(decode(:'pub_k1', 'hex')), 1, 16), 'hex') AS k1_id_is_hash_prefix,
       (SELECT count(*) FROM kingres.ledger_client_key) AS keys,
       (SELECT client_keys_active FROM kingres.ledger_status) AS active;
SELECT algorithm, role_name, label, registered_by = current_user AS by_me FROM kingres.ledger_client_key ORDER BY label;
\set VERBOSITY sqlstate
SELECT kingres.ledger_client_key_register(decode(:'pub_k1', 'hex'), NULL, 'again');
SELECT kingres.ledger_client_key_register(decode('00', 'hex'), NULL, 'short');
SELECT kingres.ledger_client_key_register(decode(repeat('01', 32), 'hex'), 'no_such_role', 'role');
SELECT kingres.ledger_client_key_register(decode(repeat('01', 32), 'hex'), NULL, repeat('x', 201));
SELECT kingres.ledger_client_key_register(NULL, NULL, 'null');
SELECT kingres.ledger_client_key_register(decode(repeat('01', 32), 'hex'), NULL, NULL);
SELECT kingres.ledger_client_key_revoke(decode('00', 'hex'), 'short');
SELECT kingres.ledger_client_key_revoke(decode(repeat('00', 16), 'hex'), 'unknown');
SELECT kingres.ledger_client_key_revoke(decode(:'k1_key_id', 'hex'), repeat('x', 201));
\set VERBOSITY terse
-- Every registration is an entry of the chain.
SELECT count(*) AS key_entries FROM kingres.ledger_entry e
  JOIN kingres.ledger_table t USING (table_id) WHERE t.relid = 'kingres.ledger_client_key'::regclass;

--------------------------------------------------------------------------
-- ledger_attest: the errors
--------------------------------------------------------------------------
\set VERBOSITY sqlstate
-- KL011: no entry yet, another operation, another hash.
BEGIN;
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK;
BEGIN;
INSERT INTO docs VALUES (1, 'one');
SAVEPOINT e;
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'D', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1_as_d', 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', decode(:'h_r2_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r2_k1', 'hex'), decode(:'s_r2_k1', 'hex'));
ROLLBACK TO e;
-- no ledger table, a table of the ledger itself, NULL arguments, malformed ones
SELECT kingres.ledger_attest('pg_class', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('kingres.ledger_attestation', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', NULL, 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', decode('00', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'X', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode('00', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode('00', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
-- KL012: a key that is not registered
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode(repeat('11', 16), 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
-- KL013: a signature of another operation, one bit changed, too short, the key of another
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1_as_d', 'hex'), decode(:'s_r1_k1_as_d', 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'),
    decode(overlay(:'s_r1_k1' placing '0' from 7 for 1), 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(substr(:'s_r1_k1', 1, 126), 'hex'));
ROLLBACK TO e;
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k3_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
ROLLBACK TO e;
ROLLBACK;
-- KL012: a key of another role
BEGIN;
INSERT INTO docs VALUES (4, 'four');
SELECT kingres.ledger_attest('docs', decode(:'h_r4_k2', 'hex'), 'I', decode(:'k2_key_id', 'hex'), decode(:'n_r4_k2', 'hex'), decode(:'s_r4_k2', 'hex'));
ROLLBACK;
SET ROLE kl_sign_other;
BEGIN;
INSERT INTO docs VALUES (4, 'four');
SELECT kingres.ledger_attest('docs', decode(:'h_r4_k2', 'hex'), 'I', decode(:'k2_key_id', 'hex'), decode(:'n_r4_k2', 'hex'), decode(:'s_r4_k2', 'hex'));
ROLLBACK;
RESET ROLE;
-- KL013: a signature made for another chain
BEGIN;
INSERT INTO docs VALUES (5, 'five');
SELECT kingres.ledger_attest('docs', decode(:'h_r5_k1_other_chain', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r5_k1_other_chain', 'hex'), decode(:'s_r5_k1_other_chain', 'hex'));
ROLLBACK;
\set VERBOSITY terse
SELECT count(*) AS attestations_after_the_errors FROM kingres.ledger_attestation;

--------------------------------------------------------------------------
-- The honest path, and a replayed nonce
--------------------------------------------------------------------------
BEGIN;
INSERT INTO docs VALUES (1, 'one');
SELECT kingres.ledger_attest('docs', decode(:'h_r1_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r1_k1', 'hex'), decode(:'s_r1_k1', 'hex'));
COMMIT;
SELECT count(*) AS attestations, count(DISTINCT key_id) AS keys FROM kingres.ledger_attestation;
-- the entry is attested once
\set VERBOSITY sqlstate
BEGIN;
INSERT INTO docs VALUES (2, 'two');
SELECT kingres.ledger_attest('docs', decode(:'h_r2_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r2_k1', 'hex'), decode(:'s_r2_k1', 'hex'));
SELECT kingres.ledger_attest('docs', decode(:'h_r2_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r2_k1', 'hex'), decode(:'s_r2_k1', 'hex'));
ROLLBACK;
-- KL014: the nonce of the first attestation, with a valid signature, for another row
BEGIN;
INSERT INTO docs VALUES (3, 'three');
SELECT kingres.ledger_attest('docs', decode(:'h_r3_k1_replay', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r3_k1_replay', 'hex'), decode(:'s_r3_k1_replay', 'hex'));
ROLLBACK;
\set VERBOSITY terse
-- the same nonce with another key is another pair (the key of the role kl_sign_app, used by that role)
SET ROLE kl_sign_app;
BEGIN;
INSERT INTO docs VALUES (4, 'four');
SELECT kingres.ledger_attest('docs', decode(:'h_r4_k2', 'hex'), 'I', decode(:'k2_key_id', 'hex'), decode(:'n_r4_k2', 'hex'), decode(:'s_r4_k2', 'hex'));
COMMIT;
RESET ROLE;
SELECT count(*) AS attestations FROM kingres.ledger_attestation;

-- A revoked key
SELECT kingres.ledger_client_key_revoke(decode(:'k3_key_id', 'hex'), 'lost');
SELECT client_keys_active AS active FROM kingres.ledger_status;
\set VERBOSITY sqlstate
BEGIN;
INSERT INTO docs VALUES (6, 'six');
SELECT kingres.ledger_attest('docs', decode(:'h_r6_k3', 'hex'), 'I', decode(:'k3_key_id', 'hex'), decode(:'n_r6_k3', 'hex'), decode(:'s_r6_k3', 'hex'));
ROLLBACK;
SELECT kingres.ledger_client_key_revoke(decode(:'k3_key_id', 'hex'), 'twice');
\set VERBOSITY terse

-- Provable like every ledger row
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT kingres.ledger_generate_digest() AS dg \gset
SELECT kingres.ledger_verify_proof(kingres.ledger_inclusion_proof('kingres.ledger_attestation', kingres.ledger_row_hash(a.*)), :'dg') AS attestation_proved
  FROM kingres.ledger_attestation a ORDER BY a.ledger_txid, a.seq LIMIT 1;
SELECT kingres.ledger_verify_proof(kingres.ledger_inclusion_proof('kingres.ledger_client_key', kingres.ledger_row_hash(k.*)), :'dg') AS key_proved
  FROM kingres.ledger_client_key k WHERE label = 'key one';
SELECT kingres.ledger_verify_proof(kingres.ledger_inclusion_proof('kingres.ledger_client_key_revocation', kingres.ledger_row_hash(r.*)), :'dg') AS revocation_proved
  FROM kingres.ledger_client_key_revocation r;
SELECT count(*) AS findings FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- The policy "required"
--------------------------------------------------------------------------
-- A row written while the policy is optional is never asked for, later.
BEGIN;
INSERT INTO docs VALUES (9, 'before the switch');
COMMIT;
SET ROLE kl_sign_other;
\set VERBOSITY sqlstate
SELECT kingres.ledger_set_signature_policy('docs', 'required');
RESET ROLE;
SET ROLE kl_sign_app;
SELECT kingres.ledger_set_signature_policy('docs', 'sometimes');
SELECT kingres.ledger_set_signature_policy('kingres.ledger_attestation', 'required');
SELECT kingres.ledger_set_signature_policy('pg_class', 'required');
SELECT kingres.ledger_set_signature_policy(NULL, 'required');
\set VERBOSITY terse
SELECT kingres.ledger_set_signature_policy('docs', 'optional');
SELECT signature_policy, signature_required_from IS NULL AS no_switch FROM kingres.ledger_table WHERE relid = 'docs'::regclass;
SELECT kingres.ledger_set_signature_policy('docs', 'required');
SELECT kingres.ledger_set_signature_policy('docs', 'required');
RESET ROLE;
SELECT signature_policy, signature_required_from IS NOT NULL AS switched FROM kingres.ledger_table WHERE relid = 'docs'::regclass;
SELECT required_tables FROM kingres.ledger_status;
\set VERBOSITY sqlstate
SET ROLE kl_sign_app;
SELECT kingres.ledger_set_signature_policy('docs', 'optional');
RESET ROLE;
\set VERBOSITY sqlstate
SELECT kingres.ledger_set_signature_policy('kingres.ledger_attestation', 'required');
\set VERBOSITY terse
-- a row without its attestation: COMMIT and PREPARE TRANSACTION fail
BEGIN;
INSERT INTO docs VALUES (7, 'seven');
COMMIT;
BEGIN;
INSERT INTO docs VALUES (7, 'seven');
PREPARE TRANSACTION 'kl_sign_p';
\set VERBOSITY terse
SELECT count(*) AS prepared, (SELECT count(*) FROM docs WHERE id = 7) AS seven FROM pg_prepared_xacts;
BEGIN;
INSERT INTO docs VALUES (7, 'seven');
SELECT kingres.ledger_attest('docs', decode(:'h_r7_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r7_k1', 'hex'), decode(:'s_r7_k1', 'hex'));
COMMIT;
SELECT count(*) AS seven FROM docs WHERE id = 7;
-- a savepoint takes the entry and its attestation with it
BEGIN;
SAVEPOINT s;
INSERT INTO docs VALUES (8, 'eight');
SELECT kingres.ledger_attest('docs', decode(:'h_r8_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r8_k1', 'hex'), decode(:'s_r8_k1', 'hex'));
ROLLBACK TO s;
COMMIT;
SELECT count(*) AS eight FROM docs WHERE id = 8;
-- the entry stays when only the attestation is rolled back
\set VERBOSITY sqlstate
BEGIN;
INSERT INTO docs VALUES (8, 'eight');
SAVEPOINT s;
SELECT kingres.ledger_attest('docs', decode(:'h_r8_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r8_k1', 'hex'), decode(:'s_r8_k1', 'hex'));
ROLLBACK TO s;
COMMIT;
\set VERBOSITY terse
BEGIN;
INSERT INTO docs VALUES (8, 'eight');
SAVEPOINT s;
SELECT kingres.ledger_attest('docs', decode(:'h_r8_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r8_k1', 'hex'), decode(:'s_r8_k1', 'hex'));
ROLLBACK TO s;
SELECT kingres.ledger_attest('docs', decode(:'h_r8_k1', 'hex'), 'I', decode(:'k1_key_id', 'hex'), decode(:'n_r8_k1_c', 'hex'), decode(:'s_r8_k1_c', 'hex'));
COMMIT;
SELECT count(*) AS eight FROM docs WHERE id = 8;
-- an UPDATE of an updatable table needs two attestations
SET ROLE kl_sign_app;
SELECT kingres.ledger_set_signature_policy('items', 'required');
RESET ROLE;
\set VERBOSITY sqlstate
BEGIN;
INSERT INTO items VALUES (1, 'x');
COMMIT;
\set VERBOSITY terse
SELECT count(*) AS items FROM items;

--------------------------------------------------------------------------
-- The guard: nobody writes the three tables but the code of the extension
--------------------------------------------------------------------------
\set VERBOSITY sqlstate
SET ROLE kl_sign_wad;
INSERT INTO kingres.ledger_attestation VALUES (1, 1, decode(repeat('11', 16), 'hex'), decode(repeat('22', 16), 'hex'), decode(repeat('33', 64), 'hex'));
INSERT INTO kingres.ledger_client_key VALUES (decode(repeat('11', 16), 'hex'), 'ed25519', decode(repeat('44', 32), 'hex'), NULL, 'forged', 'x', now());
INSERT INTO kingres.ledger_client_key_revocation VALUES (decode(:'k1_key_id', 'hex'), 'forged', 'x', now());
UPDATE kingres.ledger_client_key SET label = 'x';
DELETE FROM kingres.ledger_attestation;
DELETE FROM kingres.ledger_client_key_revocation;
TRUNCATE kingres.ledger_client_key;
RESET ROLE;
INSERT INTO kingres.ledger_attestation VALUES (1, 1, decode(repeat('11', 16), 'hex'), decode(repeat('22', 16), 'hex'), decode(repeat('33', 64), 'hex'));
UPDATE kingres.ledger_attestation SET seq = 5;
DELETE FROM kingres.ledger_client_key;
TRUNCATE kingres.ledger_attestation;
DROP TABLE kingres.ledger_attestation;
DROP TABLE kingres.ledger_client_key CASCADE;
ALTER TABLE kingres.ledger_client_key_revocation ADD COLUMN x int;
ALTER TABLE kingres.ledger_attestation DISABLE TRIGGER ALL;
GRANT INSERT ON kingres.ledger_attestation TO kl_sign_app;
CREATE INDEX ON kingres.ledger_attestation (seq);
CREATE TRIGGER x AFTER INSERT ON kingres.ledger_attestation FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify();
DROP FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea);
ALTER TABLE kingres.ledger_table DROP COLUMN signature_policy;
UPDATE kingres.ledger_table SET signature_policy = 'optional' WHERE relid = 'docs'::regclass;
GRANT UPDATE ON kingres.ledger_table TO kl_sign_app;
DROP EXTENSION kingres_ledger;
DROP SCHEMA kingres CASCADE;
\set VERBOSITY terse
SELECT count(*) AS the_tables_are_still_there FROM pg_class WHERE relname IN ('ledger_client_key', 'ledger_client_key_revocation', 'ledger_attestation');
SELECT has_table_privilege('kl_sign_app', 'kingres.ledger_attestation', 'select') AS app_reads,
       has_table_privilege('public', 'kingres.ledger_attestation', 'select') AS public_reads,
       has_table_privilege('kl_sign_wad', 'kingres.ledger_attestation', 'insert') AS wad_inserts;
SELECT count(*) AS findings FROM kingres.ledger_verify();

--------------------------------------------------------------------------
-- ledger_verify: a deleted, changed and unknown attestation
--------------------------------------------------------------------------
SELECT count(*) AS attestations FROM kingres.ledger_attestation;
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT count(*) AS findings FROM kingres.ledger_verify();
-- The report with the values that change from run to run masked.
CREATE FUNCTION public.vreport() RETURNS TABLE (check_name text, object text, detail text)
LANGUAGE sql AS $$
    SELECT v.check_name,
           regexp_replace(regexp_replace(v.object, 'tx [0-9]+', 'tx N', 'g'), 'seq [0-9]+', 'seq S', 'g'),
           regexp_replace(regexp_replace(v.detail, '[0-9a-f]{64}', '<hash>', 'g'), 'transaction [0-9]+', 'transaction N', 'g')
      FROM kingres.ledger_verify() v ORDER BY 1, 2, 3
$$;
CREATE TABLE saved_att AS TABLE kingres.ledger_attestation;
\set trig_off 'UPDATE pg_trigger SET tgenabled = ''D'' WHERE tgrelid IN (''kingres.ledger_attestation''::regclass, ''kingres.ledger_client_key''::regclass, ''kingres.ledger_client_key_revocation''::regclass) AND tgname IN (''kingres_ledger_capture'', ''kingres_ledger_block_modify'', ''kingres_ledger_block_truncate'')'
\set trig_on 'UPDATE pg_trigger SET tgenabled = ''A'' WHERE tgrelid IN (''kingres.ledger_attestation''::regclass, ''kingres.ledger_client_key''::regclass, ''kingres.ledger_client_key_revocation''::regclass) AND tgname IN (''kingres_ledger_capture'', ''kingres_ledger_block_modify'', ''kingres_ledger_block_truncate'')'

-- 1. An attestation deleted from the table.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
DELETE FROM kingres.ledger_attestation WHERE ledger_txid = (SELECT min(ledger_txid) FROM kingres.ledger_attestation);
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport();
-- put it back by hand (the triggers are off), then 2. a signature changed
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_attestation SELECT * FROM public.saved_att s
 WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_attestation a WHERE a.ledger_txid = s.ledger_txid AND a.seq = s.seq);
SELECT count(*) AS rows_again FROM kingres.ledger_attestation;
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS findings FROM kingres.ledger_verify() WHERE check_name = 'table_rows';
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_attestation SET signature = decode(repeat('00', 64), 'hex') WHERE ledger_txid = (SELECT max(ledger_txid) FROM kingres.ledger_attestation);
:trig_on;
\c
SET search_path = public;
SELECT * FROM public.vreport() WHERE check_name = 'table_rows';
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_attestation a SET signature = s.signature FROM public.saved_att s WHERE a.ledger_txid = s.ledger_txid AND a.seq = s.seq;
:trig_on;
\c
SET search_path = public;
SELECT count(*) AS findings_after_restore FROM kingres.ledger_verify() WHERE check_name = 'table_rows';

-- 3. A key row that is not the hash of its public key, and a revocation of a key that is not there.
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_client_key VALUES (decode(repeat('99', 16), 'hex'), 'ed25519', decode(repeat('44', 32), 'hex'), NULL, 'planted', 'x', now());
INSERT INTO kingres.ledger_client_key_revocation VALUES (decode(repeat('98', 16), 'hex'), 'planted', 'x', now());
:trig_on;
\c
SET search_path = public;
SELECT check_name, object, detail FROM public.vreport() WHERE check_name IN ('client_key_id', 'client_key_unknown');

-- 4. An attestation that names a key that is not registered, one for an entry that is not
--    there, and a nonce used twice (the unique index is made non-unique, as someone with
--    the catalog could).
:trig_off;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE pg_index SET indisunique = false WHERE indexrelid = 'kingres.ledger_attestation_key_nonce_key'::regclass;
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
INSERT INTO kingres.ledger_attestation
SELECT 4000000000, 0, decode(repeat('77', 16), 'hex'), nonce, signature FROM public.saved_att LIMIT 1;
INSERT INTO kingres.ledger_attestation
SELECT 4000000000, 1, key_id, nonce, signature FROM public.saved_att ORDER BY ledger_txid LIMIT 1;
:trig_on;
UPDATE pg_index SET indisunique = true WHERE indexrelid = 'kingres.ledger_attestation_key_nonce_key'::regclass;
\c
SET search_path = public;
SELECT check_name, object, detail FROM public.vreport()
 WHERE check_name IN ('attestation_duplicate', 'attestation_orphan', 'client_key_unknown') ORDER BY 1, 2, 3;

--------------------------------------------------------------------------
-- unattested_entry: a row of a required table without an attestation, from the switch on
--------------------------------------------------------------------------
-- The row 9 was written before the switch and is not reported.
SELECT count(*) AS unattested_before FROM kingres.ledger_verify() WHERE check_name = 'unattested_entry';
-- A row written while the policy was off (someone with the catalog switched it back), then dated again.
UPDATE pg_trigger SET tgenabled = 'D'
 WHERE tgrelid = 'kingres.ledger_table'::regclass AND tgname = 'kingres_ledger_block_modify';
\c
SET search_path = public;
SET kingres_ledger.allow_manual_write = on;
UPDATE kingres.ledger_table SET signature_policy = 'optional', signature_required_from = NULL WHERE relid = 'docs'::regclass;
INSERT INTO docs VALUES (50, 'no attestation');
UPDATE kingres.ledger_table SET signature_policy = 'required', signature_required_from = 0 WHERE relid = 'docs'::regclass;
UPDATE pg_trigger SET tgenabled = 'A'
 WHERE tgrelid = 'kingres.ledger_table'::regclass AND tgname = 'kingres_ledger_block_modify';
\c
SET search_path = public;
SELECT kingres.ledger_build_block() IS NOT NULL AS built;
SELECT check_name, object, detail FROM public.vreport() WHERE check_name = 'unattested_entry';
\c :orig_db
DROP DATABASE kl_sign_t;
DROP ROLE kl_sign_app, kl_sign_other, kl_sign_wad;
