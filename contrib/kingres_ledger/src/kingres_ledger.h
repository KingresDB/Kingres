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

/*
 * The subid of the lock that serialises the block builders and
 * ledger_remove_stray: a lock on the extension's own object with a sub-object
 * number that no statement of SQL uses (every DDL on an extension locks it
 * with the number 0), so no role can take or hold it but through the code of
 * the extension.
 */
#define KL_BUILDER_LOCK_SUBID	0x4B4C

/* Version of the SQL scripts that this library needs. */
#define KL_SCRIPT_VERSION		"0.3"

/* Attribute number of history_relid in kingres.ledger_table (version 0.2). */
#define KL_LEDGER_TABLE_HISTORY_ATTNO 5

/* GUC, see kingres_ledger.c. */
extern int	kl_max_block_transactions;

/* GUC: how often the launcher looks for pending transactions, in ms; 0 = off. */
extern int	kl_max_clock_skew;

extern int	kl_block_interval;

/* GUC: the longest one worker may run for one database, in ms; 0 = no limit. */
extern int	kl_worker_timeout;

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

/* worker.c */
extern void kl_worker_init(void);
extern void kl_mark_dirty(Oid dboid);

/* protect.c */
extern void kl_protect_init(void);

/* guard.c */
extern void kl_guard_init(void);
extern void kl_guard_install(void);
extern void kl_write_enter(void);
extern void kl_write_leave(void);
extern void kl_write_reset(void);
extern bool kl_manual_delete_allowed(Relation rel);
extern bool kl_pending_delete_allowed(Relation rel);
extern bool kl_insert_allowed(void);
extern bool kl_insert_is_manual(Relation rel);
extern void kl_refuse_write_rel(Relation rel, bool is_insert);
extern bool kl_write_active(void);
extern bool kl_manual_setting_on(void);
extern bool kl_manual_write_ok(void);
extern Oid	kl_extension_oid(void);

/* write.c: the functions that write the ledger, and the record of a manual write */
extern void kl_record_manual_insert(Relation rel, HeapTuple tuple);

/* capture.c */
extern void kl_record_existing_row(int32 table_id, HeapTupleHeader header);

/* kingres_ledger.c */
extern void kl_check_owner(Oid relid);

/* history.c */
extern void kl_sync_all_history_acls(Oid extowner);

#endif							/* KINGRES_LEDGER_KINGRES_LEDGER_H */
