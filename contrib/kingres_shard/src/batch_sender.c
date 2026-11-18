/*-------------------------------------------------------------------------
 *
 * batch_sender.c
 *	  The batch sender of the row router (design/shard-phase2b.md, 11.3, 11.4,
 *	  11.9).
 *
 * The sender is given rows (one Datum per column) and a destination.  It finds the
 * group that owns each row's bucket, keeps one buffer per group (the text of the
 * values, column by column, as the literal of a text[]), and sends a buffer as one
 * statement
 *
 *	INSERT INTO t (c1, ...) SELECT u.c1::type1, ... FROM unnest($1::text[], ...) AS u(c1, ...)
 *
 * when it holds insert_batch_rows rows or insert_batch_bytes bytes.  At most one batch is
 * in flight per remote group: before the next batch is sent to a group, the answer to the
 * previous one is read, so the memory is bounded by two buffers per group (the one being
 * filled and the copy that libpq holds of the batch in flight) however many rows come.
 * The batch of the group of this node runs through SPI at once.
 *
 * The transaction is the one of 2a: a connection that gets a batch is a writer
 * (kingres_shard_begin_write_xact), the commit is two-phase when two or more nodes wrote, and
 * a failure at any point is an error, which rolls everything back; the sender takes no
 * decision about the transaction.
 *
 * Every wait is interruptible: the send is non-blocking with a latch wait
 * (kingres_shard_conn_send_params), the answer is read with libpqsrv_get_result_last, and a
 * connection that has a batch in flight is registered with the pending protocol of
 * node_conn.c, so that a statement of another owner that wants the connection (the scan
 * that reads the source rows from the same node) reads the answer first, and an abort lets go.
 *
 * Placement.  Every batch statement carries the placement number the caller decided
 * with, and the group compares it with its own after planning (planner.c, 11.5).  The
 * sender reads the placement and the identity of the partitions of every group before
 * its first batch (kingres_shard_probe_before_sql) and again after the last
 * (kingres_shard_probe_after_sql); a bucket that moved in between changes one of the numbers
 * and the statement fails with KS001.  Rows are never stored in a wrong bucket whatever the
 * origin believes: the tuple routing of the group fails a row whose bucket is not attached
 * there.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/batch_sender.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/xact.h"
#include "catalog/pg_type_d.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "lib/stringinfo.h"
#include "libpq/libpq-be-fe-helpers.h"
#include "miscadmin.h"
#include "utils/builtins.h"
#include "utils/lsyscache.h"
#include "utils/memutils.h"
#include "utils/wait_classes.h"

#include "batch_sender.h"
#include "catalog.h"
#include "fanout.h"
#include "hashing.h"
#include "kingres_shard.h"
#include "node_conn.h"
#include "placement_check.h"

/* The destination of a group of rows: one node. */
typedef struct KsBatchDest
{
	KsBatchSender *s;
	int			group_id;		/* -1 for a destination that is not a group */
	int			node_id;
	char	   *host;
	int			port;
	bool		is_local;
	KsConn	   *kc;
	bool		started;		/* the writer is marked and the probe before was made */
	int64		identity;		/* the identity of the partitions, read by the probe before */
	int64		ident_given;	/* check_identity: the identity the statements carry */
	StringInfoData *cols;		/* one literal of a text[] per column */
	int			nrows;
	int64		nbytes;
	bool		in_flight;
	int			batch_no;
} KsBatchDest;

struct KsBatchSender
{
	MemoryContext cxt;
	KsDestKind	kind;
	char	   *schema;
	char	   *table;
	char	   *qualified;
	Oid			relid;
	KsColocation *placement;
	int			key_attno;
	int64		expect_placement;
	bool		check_identity;
	int			colocation_id;
	int64		origin_identity;
	bool		origin_has_parts;
	int			ncols;
	Oid		   *coltypes;
	FmgrInfo   *outfuncs;
	char	   *sql_remote_body;	/* the statement without the marker */
	char	   *sql_local;
	bool		returning;
	TupleDesc	returning_desc;
	Tuplestorestate *returning_rows;
	FmgrInfo   *ret_in;
	Oid		   *ret_ioparam;
	MemoryContext rowcxt;
	MemoryContext valcxt;		/* the text of one value: reset after every value */
	List	   *dests;			/* of KsBatchDest *, in group (or node id) order */
	KsBatchDest **by_group;
	int			ngroup_slots;
	int64		processed;
	int			nbatches;		/* batches sent so far, to name the first one */
	int			nanswered;		/* batches answered so far */
	bool		finished;
};

/* What the context line of an error in a batch says. */
typedef struct BatchErrCtx
{
	KsBatchSender *s;
	KsBatchDest *d;
} BatchErrCtx;

static void bs_reset_callback(void *arg);
static void bs_finish_pending(void *owner, bool quiet);

/* ---- the marker ---- */

bool
kingres_shard_parse_insert_marker(const char *query_string, int64 *place, bool *has_ident,
								  int64 *ident)
{
	const char *p;
	char	   *end;

	if (query_string == NULL ||
		strncmp(query_string, KS_INSERT_MARKER_PREFIX, strlen(KS_INSERT_MARKER_PREFIX)) != 0)
		return false;
	p = query_string + strlen(KS_INSERT_MARKER_PREFIX);
	if (strncmp(p, "place=", 6) != 0)
		return false;
	p += 6;
	*place = strtoll(p, &end, 10);
	if (end == p)
		return false;
	p = end;
	*has_ident = false;
	*ident = 0;
	if (strncmp(p, " ident=", 7) == 0)
	{
		p += 7;
		*ident = strtoll(p, &end, 10);
		if (end == p)
			return false;
		p = end;
		*has_ident = true;
	}
	return strncmp(p, " */ ", 4) == 0;
}

/* ---- error context ---- */

static void
batch_error_context(void *arg)
{
	BatchErrCtx *c = (BatchErrCtx *) arg;

	if (c->d->group_id >= 0)
		errcontext("bulk insert of %s, batch %d to group %d", c->s->qualified, c->d->batch_no,
				   c->d->group_id);
	else
		errcontext("bulk insert of %s, batch %d to node %d", c->s->qualified, c->d->batch_no,
				   c->d->node_id);
}

/* ---- the statement ---- */

static char *
build_statement(KsBatchSender *s, const KsBatchSpec *spec, const char *marker_fmt_unused)
{
	StringInfoData sql;
	ListCell   *lc;
	int			i;

	(void) marker_fmt_unused;
	initStringInfo(&sql);
	appendStringInfo(&sql, "INSERT INTO %s (", s->qualified);
	i = 0;
	foreach(lc, spec->columns)
	{
		KsBatchColumn *c = (KsBatchColumn *) lfirst(lc);

		appendStringInfo(&sql, "%s%s", i++ > 0 ? ", " : "", quote_identifier(c->name));
	}
	appendStringInfoString(&sql, ")");
	if (spec->overriding_system_value)
		appendStringInfoString(&sql, " OVERRIDING SYSTEM VALUE");
	appendStringInfoString(&sql, " SELECT ");
	i = 0;
	foreach(lc, spec->columns)
	{
		KsBatchColumn *c = (KsBatchColumn *) lfirst(lc);

		appendStringInfo(&sql, "%su.%s::%s", i++ > 0 ? ", " : "", quote_identifier(c->name),
						 /*
						  * TYPEMOD_GIVEN: bpchar and bit without a length must not be written as
						  * "character" and "bit", which mean character(1) and bit(1) (the value
						  * 'ab' of a character(5) column would be cut to 'a').
						  */
						 format_type_extended(c->type, -1,
											  FORMAT_TYPE_TYPEMOD_GIVEN | FORMAT_TYPE_FORCE_QUALIFY));
	}
	/*
	 * Unqualified: the parser turns only an unqualified unnest() with several arguments into the
	 * multi-array form.  Every session that runs the statement has search_path = pg_catalog
	 * (the transmission modes), so it is the built-in one.
	 */
	appendStringInfoString(&sql, " FROM unnest(");
	for (i = 0; i < s->ncols; i++)
		appendStringInfo(&sql, "%s$%d::pg_catalog.text[]", i > 0 ? ", " : "", i + 1);
	appendStringInfoString(&sql, ") AS u(");
	i = 0;
	foreach(lc, spec->columns)
	{
		KsBatchColumn *c = (KsBatchColumn *) lfirst(lc);

		appendStringInfo(&sql, "%s%s", i++ > 0 ? ", " : "", quote_identifier(c->name));
	}
	appendStringInfoString(&sql, ")");
	if (spec->conflict == KS_CONFLICT_NOTHING && spec->conflict_sql != NULL)
		appendStringInfo(&sql, " %s", spec->conflict_sql);
	if (spec->returning_sql != NULL)
		appendStringInfo(&sql, " RETURNING %s", spec->returning_sql);
	return sql.data;
}

/* ---- buffers ---- */

static void
buffers_reset(KsBatchDest *d)
{
	int			i;

	for (i = 0; i < d->s->ncols; i++)
	{
		resetStringInfo(&d->cols[i]);
		appendStringInfoChar(&d->cols[i], '{');
	}
	d->nrows = 0;
	d->nbytes = 0;
}

static KsBatchDest *
make_dest(KsBatchSender *s, int group_id, const KsNode *n, bool is_local)
{
	KsBatchDest *d = palloc0(sizeof(KsBatchDest));
	int			i;

	d->s = s;
	d->group_id = group_id;
	d->node_id = n->node_id;
	d->host = pstrdup(n->host);
	d->port = n->port;
	d->is_local = is_local;
	d->cols = palloc0(sizeof(StringInfoData) * Max(s->ncols, 1));
	for (i = 0; i < s->ncols; i++)
		initStringInfo(&d->cols[i]);
	buffers_reset(d);
	return d;
}

/* One element of a text[] literal: double-quoted, with backslash and quote escaped. */
static void
append_element(StringInfo buf, const char *v)
{
	appendStringInfoChar(buf, '"');
	for (; *v; v++)
	{
		if (*v == '"' || *v == '\\')
			appendStringInfoChar(buf, '\\');
		appendStringInfoChar(buf, *v);
	}
	appendStringInfoChar(buf, '"');
}

/* ---- begin ---- */

KsBatchSender *
kingres_shard_batch_begin(const KsDestination *dest, const KsBatchSpec *spec)
{
	MemoryContext parent = CurrentMemoryContext;
	MemoryContext cxt = AllocSetContextCreate(parent, "kingres_shard batch sender",
											  ALLOCSET_DEFAULT_SIZES);
	MemoryContext old = MemoryContextSwitchTo(cxt);
	KsBatchSender *s = palloc0(sizeof(KsBatchSender));
	const KsCatalog *cat;
	MemoryContextCallback *cb;
	ListCell   *lc;
	int			i;

	s->cxt = cxt;
	cb = palloc(sizeof(MemoryContextCallback));
	cb->func = bs_reset_callback;
	cb->arg = s;
	MemoryContextRegisterResetCallback(cxt, cb);

	s->kind = dest->kind;
	s->schema = pstrdup(dest->schema);
	s->table = pstrdup(dest->table);
	s->qualified = pstrdup(quote_qualified_identifier(dest->schema, dest->table));
	s->relid = dest->relid;
	s->key_attno = dest->key_attno;
	s->expect_placement = dest->expect_placement;
	s->check_identity = dest->check_identity;
	s->ncols = list_length(spec->columns);
	s->coltypes = palloc(sizeof(Oid) * Max(s->ncols, 1));
	s->outfuncs = palloc0(sizeof(FmgrInfo) * Max(s->ncols, 1));
	i = 0;
	foreach(lc, spec->columns)
	{
		KsBatchColumn *c = (KsBatchColumn *) lfirst(lc);
		Oid			outfunc;
		bool		isvarlena;

		s->coltypes[i] = c->type;
		getTypeOutputInfo(c->type, &outfunc, &isvarlena);
		fmgr_info(outfunc, &s->outfuncs[i]);
		i++;
	}
	if (s->ncols < 1)
		elog(ERROR, "a batch needs at least one column");

	if (spec->returning_sql != NULL)
	{
		s->returning = true;
		s->returning_desc = spec->returning_desc;
		s->returning_rows = spec->returning_rows;
		s->rowcxt = AllocSetContextCreate(cxt, "kingres_shard batch rows", ALLOCSET_DEFAULT_SIZES);
		s->ret_in = palloc0(sizeof(FmgrInfo) * s->returning_desc->natts);
		s->ret_ioparam = palloc0(sizeof(Oid) * s->returning_desc->natts);
		for (i = 0; i < s->returning_desc->natts; i++)
		{
			Oid			infunc;

			getTypeInputInfo(TupleDescAttr(s->returning_desc, i)->atttypid, &infunc,
							 &s->ret_ioparam[i]);
			fmgr_info(infunc, &s->ret_in[i]);
		}
	}

	s->valcxt = AllocSetContextCreate(cxt, "kingres_shard batch value", ALLOCSET_SMALL_SIZES);
	s->sql_remote_body = build_statement(s, spec, NULL);

	cat = kingres_shard_catalog();
	if (dest->kind == KS_DEST_PLACEMENT)
	{
		int			nb;
		int			maxgroup = -1;
		int			g;

		if (dest->placement == NULL)
			elog(ERROR, "a placement destination needs a bucket layout");
		s->placement = kingres_shard_coloc_copy(dest->placement);
		nb = kingres_shard_coloc_nbuckets(s->placement);
		for (i = 0; i < nb; i++)
			maxgroup = Max(maxgroup, kingres_shard_bucket_group(s->placement,
																kingres_shard_coloc_bucket(s->placement, i)));
		s->ngroup_slots = maxgroup + 1;
		s->by_group = palloc0(sizeof(KsBatchDest *) * Max(s->ngroup_slots, 1));
		for (i = 0; i < nb; i++)
		{
			g = kingres_shard_bucket_group(s->placement, kingres_shard_coloc_bucket(s->placement, i));
			if (g >= 0)
				s->by_group[g] = (KsBatchDest *) (intptr_t) 1;	/* a group that owns a bucket */
		}
		for (g = 0; g < s->ngroup_slots; g++)
		{
			const KsNode *node = NULL;

			if (s->by_group[g] == NULL)
				continue;
			foreach(lc, cat->nodes)
			{
				const KsNode *n = (const KsNode *) lfirst(lc);

				if (n->group_id == g && n->is_primary && n->is_active)
				{
					node = n;
					break;
				}
			}
			if (node == NULL)
				ereport(ERROR,
						(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
						 errmsg("shard group %d has no active primary node", g)));
			s->by_group[g] = make_dest(s, g, node, g == cat->local_group_id);
			s->dests = lappend(s->dests, s->by_group[g]);
		}
	}
	else if (dest->kind == KS_DEST_EVERY_NODE || dest->kind == KS_DEST_ONE_NODE)
	{
		List	   *nodes = NIL;

		if (dest->kind == KS_DEST_EVERY_NODE && OidIsValid(dest->relid))
		{
			const KsDistTable *dt = kingres_shard_lookup_dist_table(dest->relid);

			if (dt != NULL && dt->is_reference)
				ereport(ERROR,
						(errcode(ERRCODE_FEATURE_NOT_SUPPORTED),
						 errmsg("the batch sender does not write a reference table"),
						 errdetail("The writes to a reference table are the fan-out of phase 2a, which takes the writer lock and checks that the copies are equal.")));
		}
		foreach(lc, cat->nodes)
		{
			const KsNode *n = (const KsNode *) lfirst(lc);

			if (!n->is_primary || !n->is_active)
				continue;
			if (dest->kind == KS_DEST_ONE_NODE && n->node_id != dest->node_id)
				continue;
			nodes = lappend(nodes, (void *) n);
		}
		if (dest->kind == KS_DEST_ONE_NODE && nodes == NIL)
			ereport(ERROR,
					(errcode(ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE),
					 errmsg("node %d is not an active primary", dest->node_id)));
		/* in node id order, the lock-order rule of 2a */
		for (;;)
		{
			const KsNode *next = NULL;

			foreach(lc, nodes)
			{
				const KsNode *n = (const KsNode *) lfirst(lc);
				ListCell   *lc2;
				bool		done = false;

				foreach(lc2, s->dests)
					if (((KsBatchDest *) lfirst(lc2))->node_id == n->node_id)
						done = true;
				if (!done && (next == NULL || n->node_id < next->node_id))
					next = n;
			}
			if (next == NULL)
				break;
			s->dests = lappend(s->dests,
							   make_dest(s, -1, next, next->node_id == cat->local_node_id));
		}
	}

	if (OidIsValid(dest->relid))
	{
		const KsDistTable *dt = kingres_shard_lookup_dist_table(dest->relid);

		if (dt != NULL && !dt->is_reference)
		{
			s->colocation_id = dt->colocation_id;
			if (cat->local_group_id >= 0)
			{
				s->origin_has_parts = true;
				s->origin_identity = kingres_shard_partition_identity_now(dest->relid);
			}
		}
	}

	/* A transaction uses one node of a group (4.4); checked for every node before anything is sent. */
	foreach(lc, s->dests)
	{
		KsBatchDest *d = (KsBatchDest *) lfirst(lc);

		if (!d->is_local)
			kingres_shard_group_rule(d->host, d->port);
	}

	MemoryContextSwitchTo(old);
	return s;
}

/* ---- the answer to a batch ---- */

/* The first answer of the statement is a point of the fault tests. */
static void
batch_answered(KsBatchSender *s)
{
	if (s->nanswered++ == 0)
		kingres_shard_fault_point("bulk_after_first_batch");
}

/* The rows and the count of an answer; the result is cleared. */
static void
account_result(KsBatchSender *s, KsBatchDest *d, PGresult *res)
{
	(void) d;
	PG_TRY();
	{
		s->processed += strtoll(PQcmdTuples(res), NULL, 10);
		if (s->returning)
		{
			int			ntup = PQntuples(res);
			int			ncols = s->returning_desc->natts;
			Datum	   *values = palloc(sizeof(Datum) * Max(ncols, 1));
			bool	   *nulls = palloc(sizeof(bool) * Max(ncols, 1));
			int			i;
			int			j;

			if (PQnfields(res) != ncols)
				ereport(ERROR,
						(errcode(ERRCODE_DATA_EXCEPTION),
						 errmsg("remote node returned %d columns, expected %d",
								PQnfields(res), ncols)));
			for (i = 0; i < ntup; i++)
			{
				MemoryContext old;

				MemoryContextReset(s->rowcxt);
				old = MemoryContextSwitchTo(s->rowcxt);
				for (j = 0; j < ncols; j++)
				{
					if (PQgetisnull(res, i, j))
					{
						values[j] = (Datum) 0;
						nulls[j] = true;
					}
					else
					{
						nulls[j] = false;
						values[j] = InputFunctionCall(&s->ret_in[j], PQgetvalue(res, i, j),
													  s->ret_ioparam[j],
													  TupleDescAttr(s->returning_desc, j)->atttypmod);
					}
				}
				MemoryContextSwitchTo(old);
				tuplestore_putvalues(s->returning_rows, s->returning_desc, values, nulls);
			}
		}
	}
	PG_FINALLY();
	{
		PQclear(res);
	}
	PG_END_TRY();
}

/*
 * Read the answer of the batch in flight on d (the wait is interruptible: a cancel ends the
 * statement, and the abort paths find the sender still registered on the connection).  With
 * quiet an abort path asks (conn_subxact_callback, through the connection's pending owner):
 * the statement is ending, so the batch is cancelled and its answer is not read for longer than
 * a cancel request takes (kingres_shard_conn_cancel_and_drop).
 */
static void
collect(KsBatchDest *d, bool quiet)
{
	KsBatchSender *s = d->s;
	PGresult   *res;
	MemoryContext oldcxt;
	BatchErrCtx ectx;
	ErrorContextCallback errcallback;

	if (!d->in_flight)
		return;

	if (quiet)
	{
		kingres_shard_conn_cancel_and_drop(d->kc);
		d->in_flight = false;
		return;
	}

	/*
	 * The result is made in the context of the sender: on 19 a result is tied to the
	 * context it is made in, and this can be called by another statement that wants the
	 * connection, in whatever context that one is in.
	 */
	ectx.s = s;
	ectx.d = d;
	errcallback.callback = batch_error_context;
	errcallback.arg = &ectx;
	errcallback.previous = error_context_stack;
	error_context_stack = &errcallback;
	oldcxt = MemoryContextSwitchTo(s->cxt);
	kingres_shard_fault_point("bulk_before_wait");
	res = libpqsrv_get_result_last(d->kc->conn, PG_WAIT_EXTENSION);
	MemoryContextSwitchTo(oldcxt);
	kingres_shard_conn_clear_pending(d->kc, d);
	d->kc->in_query = false;
	d->in_flight = false;
	if (res == NULL)
	{
		d->kc->have_error = true;
		kingres_shard_fragment_error(d->kc, NULL, "bulk insert");
	}
	if (PQresultStatus(res) != PGRES_COMMAND_OK && PQresultStatus(res) != PGRES_TUPLES_OK)
		kingres_shard_fragment_error(d->kc, res, "bulk insert");
	error_context_stack = errcallback.previous;
	account_result(s, d, res);
	batch_answered(s);
}

/* Called when something else wants the connection while a batch is in flight. */
static void
bs_finish_pending(void *owner, bool quiet)
{
	collect((KsBatchDest *) owner, quiet);
}

/*
 * The reset callback of the sender's context: let go of every connection.  It runs whichever way
 * the statement ends, an error and a cancel included, and before the abort of the transaction
 * (the memory of a failed portal is released first).  A batch still in flight is a write that
 * nobody waits for: it is cancelled, not waited for.
 */
static void
bs_reset_callback(void *arg)
{
	KsBatchSender *s = (KsBatchSender *) arg;
	ListCell   *lc;

	foreach(lc, s->dests)
	{
		KsBatchDest *d = (KsBatchDest *) lfirst(lc);

		if (d->kc != NULL && d->in_flight)
		{
			if (d->kc->pending_owner == d)
				kingres_shard_conn_cancel_and_drop(d->kc);
			d->in_flight = false;
		}
	}
}

/* ---- sending ---- */

static void
wait_for_batch(KsBatchDest *d)
{
	if (d->in_flight)
		collect(d, false);
}

/*
 * The first use of a node by this statement: a writer, and the question about its
 * placement and the identity of its partitions (the probe before, 4.3).
 */
static void
start_dest(KsBatchSender *s, KsBatchDest *d)
{
	BatchErrCtx ectx;
	ErrorContextCallback errcallback;

	if (d->started)
		return;
	d->kc = kingres_shard_get_conn(d->host, d->port, false, NULL);
	kingres_shard_begin_write_xact(d->kc);

	ectx.s = s;
	ectx.d = d;
	errcallback.callback = batch_error_context;
	errcallback.arg = &ectx;
	errcallback.previous = error_context_stack;
	error_context_stack = &errcallback;

	kingres_shard_fault_point("bulk_before_probe");
	if (s->colocation_id != 0 && s->expect_placement != 0)
	{
		char	   *ask = kingres_shard_probe_before_sql(s->colocation_id, 1, &s->relid);
		PGresult   *res = kingres_shard_exec_params(d->kc, ask, 0, NULL, NULL, false);

		kingres_shard_probe_read_before(d->kc, res, 1, s->expect_placement, &d->identity);
	}
	else if (s->check_identity)
	{
		char	   *ask = psprintf("SELECT %s.internal_partition_identity(%s::pg_catalog.regclass)",
								   KINGRES_SHARD_SCHEMA, quote_literal_cstr(s->qualified));
		PGresult   *res = kingres_shard_exec_params(d->kc, ask, 0, NULL, NULL, false);

		if (res == NULL || PQresultStatus(res) != PGRES_TUPLES_OK)
			kingres_shard_fragment_error(d->kc, res, "identity");
		d->ident_given = PQgetisnull(res, 0, 0) ? 0 : pg_strtoint64(PQgetvalue(res, 0, 0));
		PQclear(res);
	}
	error_context_stack = errcallback.previous;
	d->started = true;
}

static void
send_batch(KsBatchSender *s, KsBatchDest *d)
{
	const char **values;
	int			i;
	char	   *sql;
	bool		sent;

	/* One batch in flight per node: the answer to the last one first. */
	wait_for_batch(d);
	start_dest(s, d);
	wait_for_batch(d);			/* the probe may have read what another owner left */

	if (s->nbatches == 0)
		kingres_shard_fault_point("bulk_before_first_batch");

	if (s->expect_placement != 0 && s->colocation_id != 0)
		sql = psprintf(KS_INSERT_MARKER_PREFIX "place=" INT64_FORMAT " */ %s",
					   s->expect_placement, s->sql_remote_body);
	else if (s->check_identity)
		sql = psprintf(KS_INSERT_MARKER_PREFIX "place=0 ident=" INT64_FORMAT " */ %s",
					   d->ident_given, s->sql_remote_body);
	else
		sql = psprintf(KS_INSERT_MARKER_PREFIX "place=0 */ %s", s->sql_remote_body);

	values = palloc(sizeof(char *) * s->ncols);
	for (i = 0; i < s->ncols; i++)
	{
		appendStringInfoChar(&d->cols[i], '}');
		values[i] = d->cols[i].data;
	}
	d->batch_no++;
	s->nbatches++;
	kingres_shard_fault_point("bulk_before_send");
	sent = kingres_shard_conn_send_params(d->kc, sql, s->ncols, values);
	buffers_reset(d);
	pfree(values);
	pfree(sql);
	if (!sent)
	{
		BatchErrCtx ectx;
		ErrorContextCallback errcallback;

		ectx.s = s;
		ectx.d = d;
		errcallback.callback = batch_error_context;
		errcallback.arg = &ectx;
		errcallback.previous = error_context_stack;
		error_context_stack = &errcallback;
		kingres_shard_fragment_error(d->kc, NULL, "bulk insert");
	}
	d->in_flight = true;
	kingres_shard_conn_set_pending(d->kc, d, bs_finish_pending);
	kingres_shard_fault_point("bulk_after_send");
}

/* The batch of this node's own group: through SPI, at once. */
static void
run_local(KsBatchSender *s, KsBatchDest *d)
{
	Datum	   *values = palloc(sizeof(Datum) * s->ncols);
	char	   *nulls = palloc(s->ncols);
	Oid		   *types = palloc(sizeof(Oid) * s->ncols);
	int			i;
	int			ret;
	int			nest;
	BatchErrCtx ectx;
	ErrorContextCallback errcallback;

	if (s->nbatches == 0)
		kingres_shard_fault_point("bulk_before_first_batch");
	for (i = 0; i < s->ncols; i++)
	{
		appendStringInfoChar(&d->cols[i], '}');
		values[i] = CStringGetTextDatum(d->cols[i].data);
		nulls[i] = ' ';
		types[i] = TEXTOID;
	}
	d->batch_no++;
	s->nbatches++;
	ectx.s = s;
	ectx.d = d;
	errcallback.callback = batch_error_context;
	errcallback.arg = &ectx;
	errcallback.previous = error_context_stack;
	error_context_stack = &errcallback;

	kingres_shard_fault_point("bulk_before_local_batch");
	if (SPI_connect() != SPI_OK_CONNECT)
		elog(ERROR, "SPI_connect failed");
	nest = kingres_shard_set_transmission_modes();
	ret = kingres_shard_spi_local_part(s->sql_remote_body, s->ncols, types, values, nulls);
	kingres_shard_reset_transmission_modes(nest);
	if (ret != SPI_OK_INSERT && ret != SPI_OK_INSERT_RETURNING)
		elog(ERROR, "the local batch of a bulk insert failed (%d)", ret);
	s->processed += SPI_processed;
	if (s->returning)
	{
		uint64		k;

		if (SPI_tuptable == NULL || SPI_tuptable->tupdesc->natts != s->returning_desc->natts)
			elog(ERROR, "unexpected result shape of a local batch");
		for (k = 0; k < SPI_processed; k++)
			tuplestore_puttuple(s->returning_rows, SPI_tuptable->vals[k]);
	}
	SPI_finish();
	batch_answered(s);
	error_context_stack = errcallback.previous;
	buffers_reset(d);
	/* the text of a batch is up to insert_batch_bytes: it must not stay until the end of the statement */
	for (i = 0; i < s->ncols; i++)
		pfree(DatumGetPointer(values[i]));
	pfree(values);
	pfree(nulls);
	pfree(types);
}

static void
flush_dest(KsBatchSender *s, KsBatchDest *d)
{
	if (d->nrows == 0)
		return;
	if (d->is_local)
		run_local(s, d);
	else
		send_batch(s, d);
}

/* ---- rows ---- */

static void
add_to_dest(KsBatchSender *s, KsBatchDest *d, const Datum *values, const bool *nulls)
{
	int			i;

	for (i = 0; i < s->ncols; i++)
	{
		StringInfo	buf = &d->cols[i];

		if (d->nrows > 0)
			appendStringInfoChar(buf, ',');
		if (nulls[i])
		{
			appendStringInfoString(buf, "NULL");
			d->nbytes += 4;
		}
		else
		{
			MemoryContext old = MemoryContextSwitchTo(s->valcxt);
			char	   *txt = OutputFunctionCall(&s->outfuncs[i], values[i]);

			MemoryContextSwitchTo(old);
			append_element(buf, txt);
			d->nbytes += strlen(txt) + 3;
			MemoryContextReset(s->valcxt);
		}
	}
	d->nrows++;
	if (d->nrows >= kingres_shard_insert_batch_rows || d->nbytes >= (int64) kingres_shard_insert_batch_bytes)
		flush_dest(s, d);
}

void
kingres_shard_batch_add(KsBatchSender *s, const Datum *values, const bool *nulls)
{
	Assert(!s->finished);
	CHECK_FOR_INTERRUPTS();
	if (s->kind == KS_DEST_PLACEMENT)
	{
		uint64		hash;
		int			bucket;
		int			g;

		if (!kingres_shard_hash_of_value(s->placement->dist_type, s->placement->collation,
										 s->placement->dist_type, values[s->key_attno],
										 nulls[s->key_attno], &hash))
			ereport(ERROR,
					(errcode(ERRCODE_DATATYPE_MISMATCH),
					 errmsg("cannot compute a bucket for a value of the distribution column")));
		bucket = kingres_shard_bucket_for_hash(s->placement, hash);
		g = kingres_shard_bucket_group(s->placement, bucket);
		if (g < 0 || g >= s->ngroup_slots || s->by_group[g] == NULL)
			elog(ERROR, "bucket %d has no owner", bucket);
		add_to_dest(s, s->by_group[g], values, nulls);
	}
	else
	{
		ListCell   *lc;

		foreach(lc, s->dests)
			add_to_dest(s, (KsBatchDest *) lfirst(lc), values, nulls);
	}
}

void
kingres_shard_batch_flush(KsBatchSender *s)
{
	ListCell   *lc;

	foreach(lc, s->dests)
		flush_dest(s, (KsBatchDest *) lfirst(lc));
	foreach(lc, s->dests)
	{
		KsBatchDest *d = (KsBatchDest *) lfirst(lc);

		if (!d->is_local)
			wait_for_batch(d);
	}
}

int64
kingres_shard_batch_finish(KsBatchSender *s)
{
	ListCell   *lc;
	int			i = 0;
	int			n = list_length(s->dests);

	kingres_shard_fault_point("bulk_before_last_flush");
	foreach(lc, s->dests)
	{
		KsBatchDest *d = (KsBatchDest *) lfirst(lc);
		bool		had_rows = d->nrows > 0;

		flush_dest(s, d);
		if (had_rows && ++i < n)
			kingres_shard_fault_point("bulk_between_groups");
	}
	foreach(lc, s->dests)
	{
		KsBatchDest *d = (KsBatchDest *) lfirst(lc);

		if (!d->is_local)
			wait_for_batch(d);
	}
	kingres_shard_fault_point("bulk_after_last_batch");

	/*
	 * The map version rule for a write (design 10.2): every group that was written is
	 * asked again for its placement and the identity of its partitions, which are changed
	 * only by the transaction of a switch; this node's own are recomputed from the
	 * catalog tables.  A bucket that moved while the rows were sent fails the statement
	 * with KS001, and nothing is committed.  A group that holds rows of this statement
	 * holds the parent in ROW EXCLUSIVE, which the switch of a move waits for.
	 */
	if (s->colocation_id != 0 && s->expect_placement != 0)
	{
		char	   *recheck = kingres_shard_probe_after_sql(s->colocation_id, 1, &s->relid);

		foreach(lc, s->dests)
		{
			KsBatchDest *d = (KsBatchDest *) lfirst(lc);
			PGresult   *res;
			BatchErrCtx ectx;
			ErrorContextCallback errcallback;

			if (d->is_local || !d->started)
				continue;
			ectx.s = s;
			ectx.d = d;
			errcallback.callback = batch_error_context;
			errcallback.arg = &ectx;
			errcallback.previous = error_context_stack;
			error_context_stack = &errcallback;
			kingres_shard_fault_point("bulk_before_recheck");
			res = kingres_shard_exec_params(d->kc, recheck, 0, NULL, NULL, false);
			kingres_shard_probe_read_after(d->kc, res, 1, s->expect_placement, &d->identity);
			error_context_stack = errcallback.previous;
		}
		kingres_shard_check_origin_placement(s->colocation_id, s->expect_placement, "writing it");
		kingres_shard_check_origin_partitions(s->relid, s->origin_has_parts, s->origin_identity);
	}
	kingres_shard_fault_point("bulk_before_commit");
	s->finished = true;
	return s->processed;
}

void
kingres_shard_batch_end(KsBatchSender *s)
{
	MemoryContextDelete(s->cxt);
}
