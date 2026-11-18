/*-------------------------------------------------------------------------
 *
 * forward_auth.c
 *	  Authentication of forwarded sessions (review finding S2-04).
 *
 * A node that forwards a statement, scans a distributed table or fans a write
 * out opens a session on another node with kingres_shard.forwarded = on.  That
 * session never forwards again and treats the statement text as a fragment of
 * the router's own statement (markers), so a client that could open such a
 * session itself would write one copy of a reference table, change a schema
 * without propagation, or write one bucket on one node.  The session therefore
 * has to prove that it comes from a node of the cluster.
 *
 * The proof is a token sent as the connection option
 * kingres_shard.forward_token: the hex form of
 *
 *	  HMAC-SHA-256(cluster secret, "kingres-forward-v2" NUL role NUL database NUL node id)
 *
 * for the role of the session, the name of the database and the id of the node
 * that receives it.  The secret is a random value made by init_cluster() and
 * kept in the table kingres.cluster_secret, on which no role has any privilege
 * and which row-level security hides from every role that is not a superuser;
 * it is read only here, with the heap API.  A token seen for one role, one
 * database or one node is useless for another; in particular a token made in a
 * copy of the database under another name (CREATE DATABASE ... TEMPLATE, a
 * restored backup) is refused by the original (S3-17).  What a copy under the
 * same name, or anyone who read the secret, can do is ended by
 * kingres.rotate_secret().  A token seen on an unprotected network can be
 * replayed for the same role, database and node, so node-to-node connections
 * belong on TLS (kingres_shard.node_conninfo, README "Securing the
 * connections").
 *
 * The token never appears anywhere a user can read: the setting rewrites its
 * own value to "<hidden>" before it is stored, so SHOW, pg_settings and
 * pg_db_role_setting-like views never hold it; no message of the extension
 * contains it; the comparison does not stop at the first different byte.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/forward_auth.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/parallel.h"
#include "access/xact.h"
#include "commands/dbcommands.h"
#include "common/hmac.h"
#include "fmgr.h"
#include "common/sha2.h"
#include "miscadmin.h"
#include "utils/acl.h"
#include "utils/builtins.h"
#include "utils/guc.h"

#include "catalog.h"
#include "forward_auth.h"
#include "kingres_shard.h"
#include "term.h"

PG_FUNCTION_INFO_V1(kingres_internal_forward_token);
PG_FUNCTION_INFO_V1(kingres_internal_gid_tag);

#define KS_SECRET_MAX		128
#define KS_TOKEN_CONTEXT	"kingres-forward-v2"

/* The GUC: its stored value is always "" or "<hidden>". */
static char *forward_token_guc = NULL;

/* What the client sent, kept out of the GUC machinery. */
static char claimed_token[KS_FORWARD_TOKEN_HEX_LEN + 1];
static size_t claimed_len = 0;

/* The claim was proved. */
static bool forwarded_verified = false;

/*
 * The check hook takes the token for itself and replaces the value that the
 * setting machinery stores (and shows, and exports to parallel workers) by a
 * placeholder.
 */
static bool
check_forward_token(char **newval, void **extra, GucSource source)
{
	if (*newval == NULL || (*newval)[0] == '\0')
		return true;
	if (strcmp(*newval, "<hidden>") == 0)
		return true;
	memset(claimed_token, 0, sizeof(claimed_token));
	claimed_len = strlen(*newval);
	strlcpy(claimed_token, *newval, sizeof(claimed_token));
	explicit_bzero(*newval, strlen(*newval));
	guc_free(*newval);
	*newval = guc_strdup(LOG, "<hidden>");
	return *newval != NULL;
}

void
kingres_shard_forward_auth_init(void)
{
	DefineCustomStringVariable("kingres_shard.forward_token",
							   "Set by the router on forwarded sessions; not for users.",
							   "Proves that a session with kingres_shard.forwarded comes from a node of the cluster.  Never shown.",
							   &forward_token_guc,
							   "",
							   PGC_BACKEND,
							   GUC_NOT_IN_SAMPLE | GUC_NO_SHOW_ALL | GUC_SUPERUSER_ONLY | GUC_NO_RESET_ALL,
							   check_forward_token, NULL, NULL);
}

/* HMAC-SHA-256 of the token context, role, database and node id, as lower-case hex. */
static bool
compute_token(const unsigned char *secret, int secret_len, const char *role,
			  const char *database, int node_id, char *hex_out)
{
	pg_hmac_ctx *ctx = pg_hmac_create(PG_SHA256);
	unsigned char mac[PG_SHA256_DIGEST_LENGTH];
	char		idbuf[16];
	bool		ok;

	if (ctx == NULL)
		return false;
	snprintf(idbuf, sizeof(idbuf), "%d", node_id);
	ok = pg_hmac_init(ctx, secret, secret_len) == 0 &&
		pg_hmac_update(ctx, (const uint8 *) KS_TOKEN_CONTEXT, sizeof(KS_TOKEN_CONTEXT)) == 0 &&
		pg_hmac_update(ctx, (const uint8 *) role, strlen(role) + 1) == 0 &&
		pg_hmac_update(ctx, (const uint8 *) database, strlen(database) + 1) == 0 &&
		pg_hmac_update(ctx, (const uint8 *) idbuf, strlen(idbuf)) == 0 &&
		pg_hmac_final(ctx, mac, sizeof(mac)) == 0;
	pg_hmac_free(ctx);
	if (!ok)
		return false;
	(void) hex_encode((const char *) mac, sizeof(mac), hex_out);
	hex_out[KS_FORWARD_TOKEN_HEX_LEN] = '\0';
	return true;
}

static char *
forward_token_of(const char *user, int node_id, bool previous)
{
	unsigned char secret[KS_SECRET_MAX];
	int			secret_len = 0;
	char	   *hex;

	if (!(previous ? kingres_shard_previous_secret(secret, sizeof(secret), &secret_len) :
		  kingres_shard_cluster_secret(secret, sizeof(secret), &secret_len)))
		return NULL;
	hex = palloc(KS_FORWARD_TOKEN_HEX_LEN + 1);
	if (!compute_token(secret, secret_len, user, get_database_name(MyDatabaseId),
					   node_id, hex))
		ereport(ERROR,
				(errcode(ERRCODE_INTERNAL_ERROR),
				 errmsg("could not compute the token for a node-to-node connection")));
	explicit_bzero(secret, sizeof(secret));
	return hex;
}

char *
kingres_shard_forward_token(const char *user, int node_id)
{
	return forward_token_of(user, node_id, false);
}

/* The token made with the previous secret, or NULL when there is none (S9-03). */
char *
kingres_shard_forward_token_previous(const char *user, int node_id)
{
	return forward_token_of(user, node_id, true);
}

/* Compare two strings of the same length in time that does not depend on where they differ. */
static bool
constant_time_equal(const char *a, const char *b, size_t len)
{
	unsigned char diff = 0;
	size_t		i;

	for (i = 0; i < len; i++)
		diff |= (unsigned char) a[i] ^ (unsigned char) b[i];
	return diff == 0;
}

static bool
session_forwarded_verified(void)
{
	unsigned char secret[KS_SECRET_MAX];
	int			secret_len = 0;
	char		expected[KS_FORWARD_TOKEN_HEX_LEN + 1];
	bool		good;

	if (!kingres_shard_forwarded)
		return false;
	if (forwarded_verified)
		return true;
	/* A parallel worker acts for a leader that has been checked. */
	if (IsParallelWorker())
		return true;
	/*
	 * An aborted transaction block (the node refused a statement of a session that has not yet
	 * proved itself, for example a node that gets the extension in add_node and has no secret
	 * yet): the only statement that gets here is ROLLBACK or COMMIT, and the secret cannot be read
	 * without a transaction (the lookup of the schema fails an assertion and is unsafe).  The claim
	 * is not proved, which is the safe answer; nothing else can run.
	 */
	if (!IsTransactionState())
		return false;

	if (!kingres_shard_cluster_secret(secret, sizeof(secret), &secret_len))
	{
		/*
		 * A node that is not part of a cluster has nothing to protect, and a
		 * node that is being added has no secret until add_node() pushes it.
		 * A node that belongs to a cluster and has none (a node upgraded from
		 * 0.1 before sync_catalog(), a node whose secret was deleted) must not
		 * believe an unproved claim (S3-09): it accepts the sessions of
		 * superusers, who can do everything anyway and are what
		 * sync_catalog() and add_node() connect as, and refuses the others.
		 * Not remembered, so that the claim is checked again once the secret
		 * arrives.
		 */
		if (kingres_shard_local_node_id_raw() == 0 ||
			superuser_arg(GetSessionUserId()))
			return true;
		ereport(FATAL,
				(errcode(ERRCODE_INVALID_AUTHORIZATION_SPECIFICATION),
				 errmsg("this session claims to have been opened by another node of the cluster but cannot prove it"),
				 errdetail("This node belongs to a cluster but holds no cluster secret to check the claim against."),
				 errhint("Run SELECT kingres.sync_catalog() on the map group primary to install the secret on every node.")));
	}

	good = claimed_len == KS_FORWARD_TOKEN_HEX_LEN &&
		compute_token(secret, secret_len,
					  GetUserNameFromId(GetSessionUserId(), false),
					  get_database_name(MyDatabaseId),
					  kingres_shard_local_node_id_raw(), expected) &&
		constant_time_equal(claimed_token, expected, KS_FORWARD_TOKEN_HEX_LEN);
	explicit_bzero(secret, sizeof(secret));
	explicit_bzero(expected, sizeof(expected));
	if (!good)
		/*
		 * The proof does not match the secret of this node (S12-05): its own SQLSTATE, KS014, tells
		 * the caller so.  The other refusal of this function, a node that holds no secret at all,
		 * keeps 28000, so that nobody decides "rebuild the node" from the words of a message.
		 */
		ereport(FATAL,
				(errcode(ERRCODE_KINGRES_SECRET_UNKNOWN),
				 errmsg("this session claims to have been opened by another node of the cluster but cannot prove it")));
	forwarded_verified = true;
	return true;
}

/*
 * Is this session a forwarded one that proved to come from a node of the
 * cluster?  A verified session also has its catalog term checked, once per
 * transaction (term.c, S7-06): a sender whose catalog has been superseded is
 * refused here, before anything it asks for runs.
 */
/* Did this session prove that it was opened by a node of the cluster? */
bool
kingres_shard_session_verified(void)
{
	return forwarded_verified;
}

/*
 * Did this session prove that it is a node session, by the token?  Not the same as
 * kingres_shard_session_forwarded(): a node that is not part of a cluster has no
 * secret to check a claim against and believes it (nothing to protect), which is
 * not enough for a function that a role could use to hold a backend (S9-09).
 */
bool
kingres_shard_session_node_proven(void)
{
	return kingres_shard_session_forwarded() && kingres_shard_session_verified();
}

bool
kingres_shard_session_forwarded(void)
{
	if (!session_forwarded_verified())
		return false;
	kingres_shard_check_claimed_term();
	return true;
}

/*
 * kingres.internal_forward_token(role text, node_id int) returns text
 *
 * For the tests of the rule: the token a node with this cluster's secret sends
 * for the role and the target node.  Superusers only (they can read the secret
 * table anyway).
 */
Datum
kingres_internal_forward_token(PG_FUNCTION_ARGS)
{
	char	   *role = text_to_cstring(PG_GETARG_TEXT_PP(0));
	char	   *token;

	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for function internal_forward_token")));
	token = kingres_shard_forward_token(role, PG_GETARG_INT32(1));
	if (token == NULL)
		PG_RETURN_NULL();
	PG_RETURN_TEXT_P(cstring_to_text(token));
}

/*
 * kingres_shard_gid_tag
 *
 * The tag of the identifier of a two-phase transaction (S7-09): the first 16
 * hex digits of HMAC-SHA-256(cluster secret, "kingres-gid-v1" NUL body).  A
 * node that holds no secret (a cluster that is not secured) uses sixteen
 * zeros, which verifies on every node of such a cluster and means nothing.  tag
 * has room for 17 bytes.  False only when the HMAC cannot be computed.
 */
static bool
gid_tag_of(const char *body, char *tag, bool previous)
{
	unsigned char secret[KS_SECRET_MAX];
	int			secret_len = 0;
	pg_hmac_ctx *ctx;
	unsigned char mac[PG_SHA256_DIGEST_LENGTH];
	char		hex[2 * PG_SHA256_DIGEST_LENGTH + 1];
	bool		ok;

	if (previous)
	{
		if (!kingres_shard_previous_secret(secret, sizeof(secret), &secret_len))
			return false;
	}
	else if (!kingres_shard_cluster_secret_for_names(secret, sizeof(secret), &secret_len))
	{
		memset(tag, '0', 16);
		tag[16] = '\0';
		return true;
	}
	ctx = pg_hmac_create(PG_SHA256);
	if (ctx == NULL)
		return false;
	ok = pg_hmac_init(ctx, secret, secret_len) == 0 &&
		pg_hmac_update(ctx, (const uint8 *) "kingres-gid-v1", sizeof("kingres-gid-v1")) == 0 &&
		pg_hmac_update(ctx, (const uint8 *) body, strlen(body)) == 0 &&
		pg_hmac_final(ctx, mac, sizeof(mac)) == 0;
	pg_hmac_free(ctx);
	explicit_bzero(secret, sizeof(secret));
	if (!ok)
		return false;
	(void) hex_encode((const char *) mac, sizeof(mac), hex);
	memcpy(tag, hex, 16);
	tag[16] = '\0';
	return true;
}

bool
kingres_shard_gid_tag(const char *body, char *tag)
{
	return gid_tag_of(body, tag, false);
}

/*
 * The tag made with the previous secret (S9-03): the name of a rotation's own
 * prepared transaction is tagged with the secret that the rotation replaces,
 * and recovery must still know it as ours once the origin holds the new one.
 * False when there is no previous secret.
 */
bool
kingres_shard_gid_tag_previous(const char *body, char *tag)
{
	return gid_tag_of(body, tag, true);
}

/*
 * kingres.internal_gid_tag(body text) returns text
 *
 * For the tests and for an operator who prepares a transaction by hand: the
 * tag that this cluster gives a name.  Superusers only.
 */
Datum
kingres_internal_gid_tag(PG_FUNCTION_ARGS)
{
	char	   *body = text_to_cstring(PG_GETARG_TEXT_PP(0));
	char		tag[17];

	if (!superuser())
		ereport(ERROR,
				(errcode(ERRCODE_INSUFFICIENT_PRIVILEGE),
				 errmsg("permission denied for function internal_gid_tag")));
	if (!kingres_shard_gid_tag(body, tag))
		ereport(ERROR,
				(errcode(ERRCODE_INTERNAL_ERROR),
				 errmsg("could not compute the tag")));
	PG_RETURN_TEXT_P(cstring_to_text(tag));
}
