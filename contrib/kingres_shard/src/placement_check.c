/*-------------------------------------------------------------------------
 *
 * placement_check.c
 *	  The placement check of design/shard-phase2b.md, 4.3, written once for every
 *	  statement that reads or writes the groups of a colocation group: the 2a
 *	  distributed scan and the fan-out write use it today, the fragment scan and
 *	  the row router of later steps use it the same way.
 *
 * What it does, in the order a statement meets it:
 *
 *	1. kingres_shard_placement_check_plan(): the placement number of the
 *	   colocation group in this backend's cache must be the one of the plan.  It
 *	   covers cached and generic plans, plans made before a move or a failover,
 *	   and a change of primary that this backend's cache has seen.
 *	2. The probe before: for every node of the statement, in one statement, the
 *	   placement of the group, the identity of the node's partitions of each
 *	   table, and the version of the extension.  The placement is the plan's, the
 *	   version is this library's, or the statement fails; the identities are kept.
 *	3. (the caller declares its cursors)
 *	4. The probe after: the placement is the plan's and every identity is what it
 *	   was.  The origin's own placement is recomputed from the catalog tables and
 *	   the identities of its own partitions are compared with the plan's.
 *
 * Nothing is returned to the client before step 4 has passed on every node.  The
 * argument that every bucket is then read once is design 10.2.  The probe
 * statements are built by two functions and compared by two others, so that
 * phase 10, whose placement epoch makes the identity redundant, changes
 * KS_PROBE_IDENTITY and nothing else.
 *
 * This file also holds the rule "one node of a group per transaction" (4.4) and
 * the error of a failed group (4.6), which every path of the extension that
 * talks to a group uses.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/placement_check.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "lib/stringinfo.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"

#include "catalog.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "placement_check.h"
#include "shippable.h"

static int
oid_cmp_for_sort(const void *a, const void *b)
{
	Oid			x = *(const Oid *) a;
	Oid			y = *(const Oid *) b;

	return (x > y) - (x < y);
}

/* The tables of a probe in the order its answer has them. */
Oid *
kingres_shard_probe_tables(int ntables, const Oid *relids)
{
	Oid		   *sorted = palloc(sizeof(Oid) * Max(ntables, 1));

	if (ntables > 0)
		memcpy(sorted, relids, sizeof(Oid) * ntables);
	qsort(sorted, ntables, sizeof(Oid), oid_cmp_for_sort);
	return sorted;
}

static char *
qualified_literal(Oid relid)
{
	return quote_literal_cstr(quote_qualified_identifier(get_namespace_name(get_rel_namespace(relid)),
														 get_rel_name(relid)));
}

/*
 * Step 1.
 */
void
kingres_shard_placement_check_plan(int colocation_id, int64 plan_placement)
{
	if (kingres_shard_placement_hash(kingres_shard_lookup_colocation(colocation_id)) != plan_placement)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("the bucket map changed after the statement was planned"),
				 errhint("A bucket moved between shard groups, or the primary of a group changed; repeat the statement.")));
}

/*
 * The next item of the list "name:version[,name:version...]" of a probe (the one place that splits it; the list was
 * checked by kingres_shard_ext_list_check, so no name has a ':' or a ','): the name and its length, and the rest of the
 * list, NULL at the end.
 */
static const char *
ext_item_next(const char *item, const char **name, size_t *namelen)
{
	const char *colon = strchr(item, ':');
	const char *comma = strchr(item, ',');

	*name = item;
	*namelen = (size_t) (colon - item);
	return comma != NULL ? comma + 1 : NULL;
}

/*
 * The question asked of every node before its cursor is opened: with the
 * parents named, so that the answer also makes the node's relcache entry of
 * each parent at least as new as the answer (kingres.internal_placement_hash).
 */
char *
kingres_shard_probe_before_sql(int colocation_id, int ntables, const Oid *relids, const char *ext)
{
	Oid		   *rels = kingres_shard_probe_tables(ntables, relids);
	StringInfoData sql;
	int			i;

	initStringInfo(&sql);
	appendStringInfoString(&sql, "SELECT ");
	for (i = 0; i < ntables; i++)
		appendStringInfo(&sql, "%s.internal_placement_hash(%d, %s::pg_catalog.regclass), ",
						 KINGRES_SHARD_SCHEMA, colocation_id, qualified_literal(rels[i]));
#if KS_PROBE_IDENTITY
	for (i = 0; i < ntables; i++)
		appendStringInfo(&sql, "%s.internal_partition_identity(%s::pg_catalog.regclass), ",
						 KINGRES_SHARD_SCHEMA, qualified_literal(rels[i]));
#endif
	appendStringInfoString(&sql,
						   "(SELECT extversion FROM pg_catalog.pg_extension WHERE extname = 'kingres_shard')");
	if (ext != NULL)
	{
		/* the extensions of the statement that the node has, "name:version" each: decided by the catalog of the node, not by the error of a parse (review P2F-01) */
		const char *p = ext;
		bool		first = true;

		kingres_shard_ext_list_check(ext);
		appendStringInfoString(&sql, ", (SELECT pg_catalog.string_agg(extname || ':' || extversion, ',' ORDER BY extname) FROM pg_catalog.pg_extension WHERE extname IN (");
		while (p != NULL)
		{
			const char *name;
			size_t		n;

			p = ext_item_next(p, &name, &n);
			appendStringInfo(&sql, "%s%s", first ? "" : ", ", quote_literal_cstr(pnstrdup(name, n)));
			first = false;
		}
		appendStringInfoString(&sql, "))");
	}
	return sql.data;
}

/* The question asked again after every cursor is open. */
char *
kingres_shard_probe_after_sql(int colocation_id, int ntables, const Oid *relids)
{
	Oid		   *rels = kingres_shard_probe_tables(ntables, relids);
	StringInfoData sql;
	int			i;

	initStringInfo(&sql);
	appendStringInfo(&sql, "SELECT %s.internal_placement_hash(%d)", KINGRES_SHARD_SCHEMA, colocation_id);
#if KS_PROBE_IDENTITY
	for (i = 0; i < ntables; i++)
		appendStringInfo(&sql, ", %s.internal_partition_identity(%s::pg_catalog.regclass)",
						 KINGRES_SHARD_SCHEMA, qualified_literal(rels[i]));
#else
	(void) i;
#endif
	return sql.data;
}

/* The number of identity columns of a probe. */
static int
probe_nidentities(int ntables)
{
#if KS_PROBE_IDENTITY
	return ntables;
#else
	return 0;
#endif
}
/*
 * The answer must be a row of the expected number of columns; anything else is a
 * node that does not run this protocol.
 */
/*
 * A number of a probe answer, read without raising: a node that answers text that is not a number
 * (a node that is not a kingres node) gives false, which the callers treat as a different
 * placement; nothing is parsed while a result is held that an error would lose (review P2B-08).
 */
static bool
probe_number(PGresult *res, int row, int col, int64 *out)
{
	char	   *end;
	const char *s;

	if (PQgetisnull(res, row, col))
		return false;
	s = PQgetvalue(res, row, col);
	errno = 0;
	*out = strtoll(s, &end, 10);
	return errno == 0 && end != s && *end == 0;
}

static void
probe_shape(KsConn *kc, PGresult *res, int ncols)
{
	if (PQntuples(res) != 1 || PQnfields(res) != ncols)
	{
		int			got = PQnfields(res);
		char	   *host = pstrdup(kc->host);
		int			port = kc->port;

		PQclear(res);
		ereport(ERROR,
				(errcode(ERRCODE_DATA_EXCEPTION),
				 errmsg("node %s:%d answered the placement probe with %d columns, expected %d",
						host, port, got, ncols)));
	}
}

void
kingres_shard_probe_read_before(KsConn *kc, PGresult *res, int ntables,
								int64 plan_placement, int64 *identities, const char *ext)
{
	int			nid = probe_nidentities(ntables);
	int			ncols = ntables + nid + 1 + (ext != NULL ? 1 : 0);
	int			i;
	bool		same = true;
	char	   *version;

	if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK)
		kingres_shard_fragment_error(kc, res, "placement");
	probe_shape(kc, res, ncols);

	/*
	 * Another version of the extension on the node: not a stale map and not
	 * something a repeat helps (rolling upgrade, design 14).  Said before the
	 * numbers, which another version may not compute the same way.
	 */
	version = PQgetisnull(res, 0, ntables + nid) ? pstrdup("") : pstrdup(PQgetvalue(res, 0, ntables + nid));
	if (strcmp(version, KINGRES_SHARD_EXT_VERSION) != 0)
	{
		char	   *host = pstrdup(kc->host);
		int			port = kc->port;

		PQclear(res);
		ereport(ERROR,
				(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
				 errmsg("node %s:%d runs kingres_shard %s, this node runs %s", host, port,
						version[0] ? version : "(none)", KINGRES_SHARD_EXT_VERSION),
				 errdetail("A statement that reads several shard groups needs every node at the same version of the extension."),
				 errhint("Finish the upgrade (ALTER EXTENSION kingres_shard UPDATE on every node); repeating the statement does not help until then.")));
	}

	/*
	 * An extension that the statement uses and the node does not have: KS001 of a version difference (design 5.5).  The groups would fail
	 * when they parse the text ("function does not exist", or "schema does not exist" when the extension was never installed); PostgreSQL
	 * has no hook between the parser and the analysis of a DECLARE, so the probe asks the catalog of the node before the DECLARE.  A node
	 * that has the extension at another version is left to the check of the group, which says both versions.
	 */
	if (ext != NULL)
	{
		const char *have = PQgetisnull(res, 0, ncols - 1) ? "" : PQgetvalue(res, 0, ncols - 1);
		const char *p = ext;

		kingres_shard_ext_list_check(ext);
		while (p != NULL)
		{
			const char *pn;
			size_t		n;
			bool		found = false;
			const char *q = have;

			p = ext_item_next(p, &pn, &n);
			while (*q)
			{
				const char *qc = strchr(q, ',');
				size_t		ql = qc != NULL ? (size_t) (qc - q) : strlen(q);

				if (ql > n && strncmp(q, pn, n) == 0 && q[n] == ':')
					found = true;
				q = qc != NULL ? qc + 1 : q + ql;
			}
			if (!found)
			{
				char	   *host = pstrdup(kc->host);
				int			port = kc->port;
				char	   *names = pstrdup(ext);

				PQclear(res);
				ereport(ERROR,
						(errcode(ERRCODE_KINGRES_STALE_MAP),
						 errmsg("node %s:%d does not have an extension that the statement uses (%s)", host, port, names),
						 errdetail("The extension %.*s is not installed in the database of that node.", (int) n, pn),
						 errhint("A repeat does not help until the extension is installed at the same version on every node; "
								 "SET kingres_shard.shippable_extensions = 'none' reads the rows and runs the extension's functions here.")));
			}
		}
	}

	for (i = 0; i < ntables; i++)
	{
		int64		v;

		if (!probe_number(res, 0, i, &v) || v != plan_placement)
			same = false;
	}
	for (i = 0; i < nid; i++)
	{
		int64		v;

		if (!probe_number(res, 0, ntables + i, &v))
			same = false;
		else if (same)
			identities[i] = v;
	}
	if (!same)
	{
		char	   *host = pstrdup(kc->host);
		int			port = kc->port;

		PQclear(res);
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("the bucket map changed while the statement was starting (node %s:%d has another placement of the buckets)",
						host, port),
				 errhint("A bucket moved between shard groups; repeat the statement.")));
	}
	PQclear(res);
}

void
kingres_shard_probe_read_after(KsConn *kc, PGresult *res, int ntables,
							   int64 plan_placement, const int64 *identities)
{
	int			nid = probe_nidentities(ntables);
	int			ncols = 1 + nid;
	int			i;
	bool		same = true;

	if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK)
		kingres_shard_fragment_error(kc, res, "placement");
	probe_shape(kc, res, ncols);
	{
		int64		v;

		if (!probe_number(res, 0, 0, &v) || v != plan_placement)
			same = false;
		for (i = 0; i < nid; i++)
			if (!probe_number(res, 0, 1 + i, &v) || v != identities[i])
				same = false;
	}
	if (!same)
	{
		/*
		 * No reload of the catalog here: it would run in this transaction,
		 * which the error ends, and the writes of the reload would wait for, and
		 * deadlock with, the very switch that made the numbers differ.
		 */
		char	   *host = pstrdup(kc->host);
		int			port = kc->port;

		PQclear(res);
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("the bucket map changed while the statement was reading it (node %s:%d has another placement of the buckets)",
						host, port),
				 errhint("A bucket moved between shard groups; repeat the statement.")));
	}
	PQclear(res);
}

/*
 * The origin is checked like a remote node: its placement is recomputed from the
 * catalog tables (kingres_shard_placement_hash_now), not read from the cache.
 */
void
kingres_shard_check_origin_placement(int colocation_id, int64 plan_placement, const char *when)
{
	if (kingres_shard_placement_hash_now(colocation_id) != plan_placement)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("the bucket map changed while the statement was %s", when),
				 errhint("A bucket moved between shard groups; repeat the statement.")));
}

/*
 * The partitions this node has now must be the ones the plan of the local part of
 * the statement was made with: the identity names the partitions themselves, so a
 * bucket that left and came back (another table with the same name) is a change.
 */
void
kingres_shard_check_origin_partitions(Oid relid, bool plan_has_parts, int64 plan_partitions)
{
	if (plan_has_parts &&
		kingres_shard_partition_identity_now(relid) != plan_partitions)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("the partitions of table \"%s\" on this node changed after the statement was planned",
						get_rel_name(relid)),
				 errhint("A bucket moved between shard groups; repeat the statement.")));
}

int64
kingres_shard_probe_placement(KsConn *kc, int colocation_id)
{
	char	   *sql = psprintf("SELECT %s.internal_placement_hash(%d)", KINGRES_SHARD_SCHEMA, colocation_id);
	PGresult   *res = kingres_shard_exec_params(kc, sql, 0, NULL, NULL, false);
	int64		theirs;

	if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK)
		kingres_shard_fragment_error(kc, res, "placement");
	theirs = 0;
	if (PQntuples(res) == 1)
		(void) probe_number(res, 0, 0, &theirs);
	PQclear(res);
	return theirs;
}

/*
 * The rule "one node of a group per transaction" (4.4).  A transaction that read
 * or wrote through the old primary of a group and then meets the new one (after
 * promote_node) would see two states of the group and would lose the writes it
 * made on the first.
 */
void
kingres_shard_group_rule(const char *host, int port)
{
	int			id;
	const KsNode *n;
	KsConn	   *other;

	if (kingres_shard_control_active())
		return;
	id = kingres_shard_conn_node_id(host, port);
	if (id == 0)
		return;
	n = kingres_shard_node_by_id(id);
	if (n == NULL)
		return;
	other = kingres_shard_conn_open_in_group(n->group_id, host, port);
	if (other != NULL)
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("this transaction already uses node %s:%d of shard group %d, and the statement needs node %s:%d of the same group",
						other->host, other->port, n->group_id, host, port),
				 errdetail("The primary of the group changed while the transaction ran; a transaction reads and writes a group through one node only."),
				 errhint("Roll back and repeat the transaction.")));
}

/*
 * Does a failure that left the node without an answer, or with this SQLSTATE,
 * come from a change of primary?  Connection loss and refusal (class 08), a
 * server that shuts down or starts (57P01, 57P02, 57P03), a node that became
 * read-only (25006), and a KS001 of the group itself.
 */
static bool
failover_failure(KsConn *kc, PGresult *res, int sqlstate)
{
	if (res == NULL)
		return true;
	/* an error that libpq makes of a connection that was closed has no SQLSTATE */
	if (sqlstate == 0)
		return kc->conn == NULL || PQstatus(kc->conn) != CONNECTION_OK;
	return ERRCODE_TO_CATEGORY(sqlstate) == ERRCODE_CONNECTION_EXCEPTION ||
		sqlstate == ERRCODE_ADMIN_SHUTDOWN || sqlstate == ERRCODE_CRASH_SHUTDOWN ||
		sqlstate == ERRCODE_CANNOT_CONNECT_NOW ||
		sqlstate == ERRCODE_READ_ONLY_SQL_TRANSACTION ||
		sqlstate == ERRCODE_KINGRES_STALE_MAP;
}

/*
 * Is the node of a failed statement in a group where a change of primary is
 * possible: a standby is registered for the group, or the node is no longer its
 * group's primary in the cache.
 */
static bool
failover_possible(const char *host, int port)
{
	const KsCatalog *cat = kingres_shard_catalog();
	const KsNode *me = NULL;
	ListCell   *lc;

	foreach(lc, cat->nodes)
	{
		const KsNode *n = (const KsNode *) lfirst(lc);

		if (n->port == port && strcmp(n->host, host) == 0)
			me = n;
	}
	if (me == NULL)
		return false;
	if (!me->is_primary || !me->is_active)
		return true;
	foreach(lc, cat->nodes)
	{
		const KsNode *n = (const KsNode *) lfirst(lc);

		if (n->group_id == me->group_id && !n->is_primary && n->is_active)
			return true;
	}
	return false;
}

void
kingres_shard_fragment_error(KsConn *kc, PGresult *res, const char *what)
{
	int			sqlstate = kingres_shard_result_sqlstate(res);
	bool		lost = (res == NULL);

	if (failover_failure(kc, res, sqlstate) && failover_possible(kc->host, kc->port))
	{
		char	   *host = pstrdup(kc->host);
		int			port = kc->port;
		char	   *said = NULL;
		const char *s;

		if (res != NULL && (s = PQresultErrorField(res, PG_DIAG_MESSAGE_PRIMARY)) != NULL)
			said = pstrdup(s);
		if (PQstatus(kc->conn) != CONNECTION_OK)
			kc->have_error = true;
		if (res != NULL)
			PQclear(res);
		ereport(ERROR,
				(errcode(ERRCODE_KINGRES_STALE_MAP),
				 errmsg("node %s:%d failed while a distributed statement was running (%s)", host, port, what),
				 errdetail("A failover of its shard group may be in progress%s%s.",
						   said ? ": the node said: " : "", said ? said : ""),
				 errhint("Repeat the statement.")));
	}

	if (lost)
	{
		char	   *host = pstrdup(kc->host);
		int			port = kc->port;

		kc->have_error = true;
		ereport(ERROR,
				(errcode(ERRCODE_CONNECTION_FAILURE),
				 errmsg("lost the connection to node %s:%d during a distributed statement (%s)",
						host, port, what),
				 errdetail_internal("%s", kc->conn != NULL ? kingres_shard_conn_error_text(kc->conn) : "no connection")));
	}
	kingres_shard_report_error(kc, res, what);
	/* report_error raises; this keeps the compiler honest about noreturn */
	elog(ERROR, "unreachable");
}
