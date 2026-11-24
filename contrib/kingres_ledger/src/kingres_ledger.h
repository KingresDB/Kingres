/*-------------------------------------------------------------------------
 *
 * kingres_ledger.h
 *	  Declarations shared by the files of the kingres_ledger extension.
 *
 * The extension implements the ledger described in
 * kingres/docs/design/ledger-mvp.md: canonical row hashing, RFC 6962
 * Merkle trees, the insert trigger, the ProcessUtility protection hook and
 * the offline proof check.  This header holds the constants, the SQLSTATE
 * codes, the big-endian helpers used to build hash inputs, and the
 * cross-file entry points.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/kingres_ledger.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_LEDGER_KINGRES_LEDGER_H
#define KINGRES_LEDGER_KINGRES_LEDGER_H

/* The differences between the PostgreSQL majors; see the file. */
#include "pg_compat.h"
#include "access/htup.h"
#include "datatype/timestamp.h"
#include "utils/relcache.h"

/* Version of the hash formats implemented by this code (see README). */
#define KL_HASH_VERSION 1

/* Length of a SHA-256 value in bytes. */
#define KL_HASH_LEN 32

/* Longest user name in UTF-8 that is hashed (NAMEDATALEN - 1 bytes of up to 4 bytes each). */
#define KL_MAX_USER_NAME_BYTES 255

/*
 * Domain tags of hash format version 1.  Every kind of hashed data starts
 * with its own tag, so that data of one kind can never be mistaken for data
 * of another: the first byte of the leaf data of a row, of an entry of a
 * transaction tree and of a transaction in a block tree, the first byte of the
 * input of a block hash, and the first byte of the input of the hash that
 * derives the previous-block hash of block 0 from the chain identity.  The
 * values avoid 0x00 and 0x01, the RFC 6962 prefixes of leaves and nodes.
 * The README has the same table.
 */
#define KL_TAG_ROW			0x10
#define KL_TAG_TX_ENTRY		0x11
#define KL_TAG_BLOCK_TX		0x12
#define KL_TAG_BLOCK		0x13
#define KL_TAG_CHAIN		0x14

/* Operations of a ledger entry: the byte hashed into the entry leaf. */
#define KL_OP_INSERT		'I'
#define KL_OP_DELETE		'D'

/* Columns that the history table adds to the columns of an updatable table. */
#define KL_HIST_COL_TXID	"ledger_txid"
#define KL_HIST_COL_SEQ		"ledger_seq"
#define KL_HIST_COL_OP		"ledger_op"

/* Length of the random identity of a chain, in bytes. */
#define KL_CHAIN_ID_LEN 32

/*
 * SQLSTATE codes of the extension.  "KL" is not a class used by PostgreSQL;
 * class names starting with a letter other than 0-4 and A-H are free for
 * user-defined codes.
 */
#define ERRCODE_KL_APPEND_ONLY	MAKE_SQLSTATE('K','L','0','0','1')
#define ERRCODE_KL_PROTECTED	MAKE_SQLSTATE('K','L','0','0','2')
#define ERRCODE_KL_NOT_PRELOADED MAKE_SQLSTATE('K','L','0','0','3')
#define ERRCODE_KL_CLOCK_SKEW	MAKE_SQLSTATE('K','L','0','0','4')
#define ERRCODE_KL_SCRIPT_VERSION MAKE_SQLSTATE('K','L','0','0','5')
#define ERRCODE_KL_LOCK_ORDER	MAKE_SQLSTATE('K','L','0','0','6')

/* Client signatures (sign.c, capture.c); KL007 to KL009 are free. */
#define ERRCODE_KL_UNATTESTED	MAKE_SQLSTATE('K','L','0','1','0')
#define ERRCODE_KL_NO_ENTRY		MAKE_SQLSTATE('K','L','0','1','1')
#define ERRCODE_KL_KEY			MAKE_SQLSTATE('K','L','0','1','2')
#define ERRCODE_KL_SIGNATURE	MAKE_SQLSTATE('K','L','0','1','3')
#define ERRCODE_KL_NONCE		MAKE_SQLSTATE('K','L','0','1','4')

/*
 * Names used to find the extension and the objects it creates.  Functions and
 * triggers of the extension are never recognised by these names alone: the
 * protection code uses extension membership (pg_depend) and, to tell the
 * functions apart, the name of the C symbol (prosrc), which a rename of the
 * SQL function does not change.
 */
#define KL_EXTENSION_NAME		"kingres_ledger"
#define KL_SCHEMA_NAME			"kingres"
#define KL_TRIGGER_CAPTURE		"kingres_ledger_capture"
#define KL_TRIGGER_BLOCK_MODIFY	"kingres_ledger_block_modify"
#define KL_TRIGGER_BLOCK_TRUNCATE "kingres_ledger_block_truncate"
#define KL_TRIGGER_GUARD_WRITE "kingres_ledger_guard_write"

/* C symbols of the functions that the protection code has to recognise. */
#define KL_SYM_CAPTURE			"kingres_ledger_capture"
#define KL_SYM_BLOCK_MODIFY		"kingres_ledger_block_modify"
#define KL_SYM_REGISTER_TABLE	"kingres_ledger_register_table"
#define KL_SYM_RECORD_ROW		"kingres_ledger_record_row"
#define KL_SYM_LOCK_TABLE		"kingres_ledger_lock_table"
#define KL_SYM_HISTORY_SYNC		"kingres_ledger_history_sync"
#define KL_SYM_HISTORY_ACL_SYNC	"kingres_ledger_history_acl_sync"
#define KL_SYM_REGISTER_INTERNAL "kingres_ledger_register_internal"

/*
 * The subid of the lock that serialises the block builders and
 * ledger_remove_stray: a lock on the extension's own object with a sub-object
 * number that no statement of SQL uses (every DDL on an extension locks it
 * with the number 0), so no role can take or hold it but through the code of
 * the extension.
 */
#define KL_BUILDER_LOCK_SUBID	0x4B4C

/*
 * Not a lock of the extension: the mark of the lock that ledger_set_signature_policy
 * holds on the table whose policy it switches (SHARE ROW EXCLUSIVE, which
 * writers of the table wait for).  It stands in the list of held locks so that
 * the timer of the hold limit covers it for a role that is not a superuser, and
 * it comes first in the order of the locks, like the lock on the table of
 * ledger_enable.  Nothing waits on this number.
 */
#define KL_POLICY_HOLD_SUBID	0x4B4B

/*
 * The locks that serialise the allocation of the identifiers of the registry
 * (KL_REGISTRY_LOCK_SUBID: ledger_enable and ledger_register_table),
 * of the recorded verifications (KL_VERIFY_LOCK_SUBID) and of the records of
 * removals and manual writes (KL_REMOVED_LOCK_SUBID).  The identifiers of the
 * recorded digests are taken under the lock of the builders.  Same kind of lock
 * as the builders' (the extension's own object, a sub-object number that no
 * statement uses).  Nothing takes an identifier from a sequence: a role that
 * holds nextval and setval on a sequence (every member of pg_write_all_data
 * does) could stop the writers by moving it, and no hook sees a call of setval.
 *
 * The order of the locks, for every writer: a lock on the table of ledger_enable,
 * then the builders', the registry's, the verifications', the removals' (the
 * numbers below are in that order).  A transaction that takes more than one takes
 * the earlier first; a write by hand takes all four (kl_lock_for_manual_write).
 * The order is enforced across calls: a call that would take a lock that is
 * earlier in the order than one the transaction holds fails at once, with
 * ERRCODE_KL_LOCK_ORDER (write.c, kl_check_lock_order).
 */
#define KL_REGISTRY_LOCK_SUBID	0x4B4D
#define KL_VERIFY_LOCK_SUBID	0x4B4E
#define KL_REMOVED_LOCK_SUBID	0x4B4F

/* Version of the SQL scripts that this library needs. */
#define KL_SCRIPT_VERSION		"0.3"

/* Attribute number of history_relid in kingres.ledger_table (version 0.2). */
#define KL_LEDGER_TABLE_HISTORY_ATTNO 5

/*
 * The tables of the extension that are ledger tables themselves (part B of
 * 0.3, client signatures): registered in kingres.ledger_table by the script of
 * the extension, written by the code of the extension only.
 */
#define KL_TABLE_CLIENT_KEY		"ledger_client_key"
#define KL_TABLE_CLIENT_REVOKE	"ledger_client_key_revocation"
#define KL_TABLE_ATTESTATION	"ledger_attestation"

/* Length of a client key identifier, nonce, signature and public key, in bytes. */
#define KL_CLIENT_KEY_ID_LEN	16
#define KL_CLIENT_NONCE_LEN		16
#define KL_CLIENT_SIG_LEN		64
#define KL_CLIENT_PUBKEY_LEN	32

/* Longest label and revocation reason, in characters. */
#define KL_CLIENT_TEXT_MAX		200

/* GUC, see kingres_ledger.c. */
extern int	kl_max_block_transactions;

/* GUC: how often the launcher looks for pending transactions, in ms; 0 = off. */
extern int	kl_max_clock_skew;

extern int	kl_block_interval;

/* GUC: the longest one worker may run for one database, in ms; 0 = no limit. */
extern int	kl_worker_timeout;

/*
 * GUC: how long a role that is not a superuser may hold one of the locks of
 * the extension (the builders', the registry's, the verifications'), in ms,
 * counted from the moment it got the lock; 0 = no limit.  A session that is
 * still in the same transaction when the time is up is ended.  Only a
 * superuser can change it.
 */
extern int	kl_lock_hold_limit;

/*
 * GUC: the most time, in ms, that one transaction of a role that is not a
 * superuser may be credited to kingres_ledger.lock_hold_limit for the entering
 * of rows by ledger_enable; 0 = no bound.  Only a superuser can change it.
 */
extern int	kl_enable_credit_limit;

/* GUC, for the tests: ms that ledger_register_table waits after each batch of rows. */
extern int	kl_enable_batch_delay;

/* True when the library was loaded through shared_preload_libraries. */
extern bool kl_preloaded;

/* Big-endian encoders used for every hash input. */
static inline void
kl_put_u16(uint8 *p, uint16 v)
{
	p[0] = (uint8) (v >> 8);
	p[1] = (uint8) v;
}

static inline void
kl_put_u32(uint8 *p, uint32 v)
{
	p[0] = (uint8) (v >> 24);
	p[1] = (uint8) (v >> 16);
	p[2] = (uint8) (v >> 8);
	p[3] = (uint8) v;
}

static inline void
kl_put_u64(uint8 *p, uint64 v)
{
	kl_put_u32(p, (uint32) (v >> 32));
	kl_put_u32(p + 4, (uint32) v);
}

/* capture.c */
extern void kl_capture_init(void);

/* capture.c */
extern void kl_seed_required(int32 table_id, const int32 *seqs, int nseqs);

/* worker.c */
extern void kl_worker_init(void);
extern void kl_mark_dirty(Oid dboid);

/* protect.c */
extern void kl_protect_init(void);

/* sign.c */
extern bool kl_is_internal_table_name(const char *name);
extern bool kl_internal_stmt_active(const char *queryString);

/* guard.c */
extern void kl_guard_init(void);
extern bool kl_policy_update_allowed(Relation rel, HeapTuple oldtup, HeapTuple newtup);
extern void kl_guard_install(void);
extern void kl_write_enter(void);
extern void kl_write_leave(void);
extern void kl_write_reset(void);
extern bool kl_manual_delete_allowed(Relation rel);
extern bool kl_pending_delete_allowed(Relation rel);
extern bool kl_insert_allowed(void);
extern bool kl_insert_is_manual(Relation rel);
extern bool kl_insert_by_hand(void);
extern void kl_refuse_write_rel(Relation rel, bool is_insert);
extern bool kl_write_active(void);
extern bool kl_manual_setting_on(void);
extern bool kl_manual_write_ok(void);
extern Oid	kl_extension_oid(void);
extern Oid	kl_catalog_relid(const char *relname);
extern Oid	kl_catalog_index_relid(const char *idxname, Oid tableoid);
extern bool kl_registry_untrusted(void);
extern bool kl_fail_closed(void);
extern void kl_require_trusted_registry(const char *what);

/*
 * The SPI statements of the extension (capture.c, row_hash.c, sign.c, write.c, worker.c) name the
 * tables of the schema "kingres" and its functions qualified and with typed arguments.  The tables
 * are the extension's own, because their names are taken from the install script on and cannot be
 * made again, and the functions of the extension cannot be replaced by another of the same
 * signature; the one thing a schema of somebody else allows is an overload of another signature,
 * which the install script refuses once the install-time check of the sec-ext branch is merged (the schema must be a superuser's and closed to everybody else).  The
 * code that reads the registry by name and acts on what it finds, and the block builder, go through
 * kl_catalog_relid() above, and where the registry cannot be trusted the guards fail closed.
 */

/* write.c: the functions that write the ledger, and the record of a manual write */
extern void kl_record_manual_insert(Relation rel, HeapTuple tuple);
extern void kl_lock_extension_object(int subid);
extern void kl_lock_for_manual_write(Relation rel);
extern HeapTuple kl_manual_write_identifier(Relation rel, HeapTuple tuple);
extern void kl_hold_credit(TimestampTz began);
extern void kl_lock_table_for_policy(Oid relid);
extern void kl_enable_batch_pause(void);
extern void kl_require_own_statement(const char *fname);
extern void kl_check_lock_order(int subid);
extern void kl_record_manual_digest_delete(Relation rel, HeapTuple tuple);
extern int	kl_run(const char *sql, int nargs, const Oid *argtypes, Datum *values,
				   const char *nulls, bool guarded);
extern bool kl_first(int col, Datum *value);
extern void kl_follow_sequence(const char *table, const char *column, int64 id);

/* capture.c */
extern void kl_record_existing_row(int32 table_id, HeapTupleHeader header);
extern void kl_attest_mark(int32 seq);

/* kingres_ledger.c */
extern void kl_check_owner(Oid relid);

/* history.c */
extern void kl_sync_all_history_acls(Oid extowner);

#endif							/* KINGRES_LEDGER_KINGRES_LEDGER_H */
