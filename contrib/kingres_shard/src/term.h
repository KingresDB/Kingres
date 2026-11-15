/*-------------------------------------------------------------------------
 *
 * term.h
 *	  The catalog term (review S7-06): a number in the catalog that every
 *	  promotion raises, carried by every catalog push and every remote
 *	  transaction of a node, so that a node that holds an older catalog than
 *	  its peer is found out, told, and cannot override the newer one.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/term.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_TERM_H
#define KINGRES_SHARD_TERM_H

#include "postgres.h"

/* The settings a remote transaction carries; see term.c. */
extern void kingres_shard_term_init(void);

/*
 * Called by every statement of a verified forwarded session.  Once per
 * transaction: refuses (KS002) a sender whose term is older than this node's,
 * and brings this node up to date first when the sender's is newer.
 */
extern void kingres_shard_check_claimed_term(void);

/*
 * A refusal with the newer term (KS002) came back from node peer_node_id (0:
 * unknown).  A node that is the map group primary according to its own
 * catalog records that it has been superseded; any other node fetches the
 * newer catalog from the peer.  Returns after the worker has ended when wait
 * is true; with wait false (the caller holds catalog locks the worker needs)
 * only the shared flag is set before returning.
 */
extern void kingres_shard_learn_term(int64 term, int peer_node_id, bool wait);

/* The worker body of "learn" (recovery.c calls it with the worker's extra text). */
extern void kingres_shard_term_worker_run(const char *extra);

/* The text of the argument of a worker request: "<term> <peer node id>". */
extern bool kingres_shard_parse_term_detail(const char *detail, int64 *term);

/* The text that follows "SET LOCAL" in the BEGIN batch of a remote transaction. */
extern void kingres_shard_append_term_claim(StringInfo buf);

/* Raises an error unless this node may act as the map group primary. */
extern void kingres_shard_refuse_if_superseded(void);

#endif							/* KINGRES_SHARD_TERM_H */
