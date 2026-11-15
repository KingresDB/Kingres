/*-------------------------------------------------------------------------
 *
 * forward_auth.h
 *	  Authentication of the sessions that one node of the cluster opens on
 *	  another (review finding S2-04).
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/forward_auth.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_FORWARD_AUTH_H
#define KINGRES_SHARD_FORWARD_AUTH_H

#include "postgres.h"

/* Length of a token in characters: the hex form of an HMAC-SHA-256. */
#define KS_FORWARD_TOKEN_HEX_LEN	(2 * 32)

extern void kingres_shard_forward_auth_init(void);

/*
 * The token a node sends when it opens a session as role "user" on the node
 * whose id is node_id, or NULL when this node holds no cluster secret (a
 * cluster that is not initialized or not yet secured).  The result is palloc'd.
 */
extern char *kingres_shard_forward_token(const char *user, int node_id);

/*
 * Is this session a forwarded session, one opened by another node of the
 * cluster?  A client may ask for it with the connection option
 * kingres_shard.forwarded, but it is believed only if it also sent the token
 * of kingres_shard.forward_token for its role and this node, which only a node
 * that knows the cluster secret can compute.  The check is made at the first
 * statement of the session (nothing can be read from the catalog while the
 * connection options are processed); a session that claims and fails to prove
 * is ended with a FATAL error before it runs anything.  Fragment markers in
 * statement text (KS_FRAGMENT_MARKER, KS_WRITE_MARKER) are looked at only in
 * a session for which this returns true.
 *
 * A node that is not part of a cluster (no local node id) holds no secret and
 * accepts the claim; so does a node that belongs to a cluster and lost its
 * secret, but only for the sessions of superusers (what add_node() and
 * sync_catalog() connect as); it refuses everyone else (S3-09).  In both cases
 * the result is not remembered, so that the claim is checked again once the
 * secret arrives (add_node installs it in the middle of a session).
 */
extern bool kingres_shard_session_forwarded(void);

/* Has the claim been proved (and not only made)?  Never raises an error. */
extern bool kingres_shard_session_verified(void);

/* The tag of a two-phase transaction identifier (16 hex digits, tag has room for 17 bytes); see forward_auth.c. */
extern bool kingres_shard_gid_tag(const char *body, char *tag);

#endif							/* KINGRES_SHARD_FORWARD_AUTH_H */
