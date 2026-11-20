/*-------------------------------------------------------------------------
 *
 * pg_compat.h
 *	  The differences between the PostgreSQL majors that kingres_shard
 *	  builds against (18 and 19), in one place.
 *
 * One source builds against every supported major (ADR 0023).  Code outside
 * this file does not test PG_VERSION_NUM: it uses what this file defines.
 * Where a name or a signature changed, the definition here is the one the
 * source uses, with a body for each major.  When a major is dropped, its
 * branch of each #if goes.
 *
 * Every definition says which upstream change it follows.  The header is
 * included by kingres_shard.h, after postgres.h, so every file has it.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/pg_compat.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_PG_COMPAT_H
#define KINGRES_SHARD_PG_COMPAT_H

/*
 * INT_MAX and friends: PostgreSQL 19's utils/guc.h still uses INT_MAX
 * (MAX_KILOBYTES) but no longer brings in <limits.h> for it.
 */
#include <limits.h>

#include "access/tableam.h"
#include "nodes/parsenodes.h"
#include "nodes/pathnodes.h"
#include "nodes/plannodes.h"
#include "nodes/primnodes.h"
#include "optimizer/planner.h"
#include "utils/lsyscache.h"

/*
 * The libpq header for backend code.  PostgreSQL 19 turns libpq use in the
 * backend into a wrapper API: libpq/libpq-be-fe.h defines PGresult, PQclear,
 * PQntuples and the other result functions as libpqsrv_ versions that act on
 * a result wrapped in a memory-context-safe object.  Every file that touches
 * a result must see the same definitions, or a wrapped result is read as a
 * plain one (the first file to include libpq-be-fe-helpers.h after
 * libpq-fe.h is not enough); node_conn.h gets this header from here, and
 * every file that uses libpq includes node_conn.h.  On 18 the plain header is
 * the one.
 */
#if PG_VERSION_NUM >= 190000
#include "libpq/libpq-be-fe.h"
#else
#include "libpq-fe.h"
#endif

/*
 * get_database_name() is declared in utils/lsyscache.h from PostgreSQL 19 on
 * (it was in commands/dbcommands.h); including lsyscache.h here gives both
 * majors the declaration.
 */

/*
 * KS_SERVER_MAJOR
 *		The major version of PostgreSQL this library was built for (18, 19, ...),
 *		the number that the checks which keep one major in a cluster compare
 *		(fault.c, node_conn.c).  Defined here so that no other file looks at the
 *		version macros.
 */
#define KS_SERVER_MAJOR (PG_VERSION_NUM / 10000)

/*
 * KS_TABLE_BEGINSCAN
 *		Start a heap scan with the default options.  PostgreSQL 19 gives
 *		table_beginscan() a "flags" argument (a bitmask of ScanOptions).
 */
#if PG_VERSION_NUM >= 190000
#define KS_TABLE_BEGINSCAN(rel, snapshot, nkeys, key) \
	table_beginscan((rel), (snapshot), (nkeys), (key), 0)
#else
#define KS_TABLE_BEGINSCAN(rel, snapshot, nkeys, key) \
	table_beginscan((rel), (snapshot), (nkeys), (key))
#endif


/*
 * The statement that CLUSTER becomes.  PostgreSQL 19 replaces ClusterStmt by
 * RepackStmt, which also stands for the new REPACK command (CLUSTER and
 * REPACK need the same guard: both rewrite the table of this node).
 * VACUUM FULL is still a VacuumStmt.  The relation is NULL when the
 * statement names none.
 */
#if PG_VERSION_NUM >= 190000
#define KS_T_CLUSTER_STMT T_RepackStmt

static inline RangeVar *
ks_cluster_stmt_relation(Node *parsetree)
{
	RepackStmt *stmt = (RepackStmt *) parsetree;

	return stmt->relation ? stmt->relation->relation : NULL;
}
#else
#define KS_T_CLUSTER_STMT T_ClusterStmt

static inline RangeVar *
ks_cluster_stmt_relation(Node *parsetree)
{
	return ((ClusterStmt *) parsetree)->relation;
}
#endif

/*
 * The planner hook of PostgreSQL 19 gets the ExplainState of an EXPLAIN as a
 * fifth argument (NULL for other statements), and standard_planner() takes
 * it too.  The hook keeps it for the call to the next planner in the chain
 * (KS_PLANNER_ES_ARG); on 18 there is no such argument and the macros vanish.
 */
#if PG_VERSION_NUM >= 190000
#define KS_PLANNER_ES_PARAM , ExplainState *es
#define KS_PLANNER_ES_VALUE (es)
#define KS_PLANNER_ES_ARG(saved) , (ExplainState *) (saved)
#else
#define KS_PLANNER_ES_PARAM
#define KS_PLANNER_ES_VALUE NULL
#define KS_PLANNER_ES_ARG(saved)
#endif

/*
 * KS_CLEAR_RESULT_RELATIONS
 *		Say that a PlannedStmt has no result relations.  PostgreSQL 19
 *		replaces the list of range table indexes PlannedStmt.resultRelations
 *		by the set resultRelationRelids.
 */
#if PG_VERSION_NUM >= 190000
#define KS_CLEAR_RESULT_RELATIONS(pstmt) ((pstmt)->resultRelationRelids = NULL)
#else
#define KS_CLEAR_RESULT_RELATIONS(pstmt) ((pstmt)->resultRelations = NIL)
#endif

/*
 * ks_result_relations
 *		The range table indexes of the relations a PlannedStmt writes (NIL for
 *		a read-only plan), as an integer list.  PostgreSQL 19 keeps them as the
 *		set PlannedStmt.resultRelationRelids instead of the list resultRelations.
 */
#if PG_VERSION_NUM >= 190000
static inline List *
ks_result_relations(PlannedStmt *pstmt)
{
	List	   *rels = NIL;
	int			rti = -1;

	while ((rti = bms_next_member(pstmt->resultRelationRelids, rti)) >= 0)
		rels = lappend_int(rels, rti);
	return rels;
}
#else
#define ks_result_relations(pstmt) ((pstmt)->resultRelations)
#endif

/*
 * The type of the notice receiver that is given to PQsetNoticeReceiver().
 * PostgreSQL 19's libpq-be-fe-helpers.h redefines the name PGresult as the
 * wrapper libpqsrv_PGresult, but libpq calls a receiver with the real one;
 * "struct pg_result" is the real one on every major.
 */
typedef struct pg_result KsRawResult;

/*
 * Who frees a libpq result?  On 18 the code that got it, with PQclear().  On
 * 19 the wrapper object of libpq-be-fe.h also frees it, when the memory
 * context it was made in is reset or deleted, so a reset callback that
 * clears results (dist_scan.c) would clear them a second time.
 */
#if PG_VERSION_NUM >= 190000
#define KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT true
#else
#define KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT false
#endif

/*
 * Which ON CONFLICT actions a statement may have to be shipped to another node.
 * PostgreSQL 19 adds ON CONFLICT DO SELECT (ONCONFLICT_SELECT), which returns
 * rows of the conflicting tuples and is not designed for this extension yet: it
 * is refused until it is.  The list names the two actions that exist on every
 * major (and ONCONFLICT_NONE, which a statement without ON CONFLICT may carry),
 * so a new one is refused whatever the major.
 */
static inline bool
ks_onconflict_action_shippable(OnConflictAction action)
{
	return action == ONCONFLICT_NONE || action == ONCONFLICT_NOTHING ||
		action == ONCONFLICT_UPDATE;
}

/*
 * KS_ONCONFLICT_IS_SELECT
 *		Is the ON CONFLICT clause a DO SELECT?  PostgreSQL 19 adds the action
 *		ONCONFLICT_SELECT (and OnConflictExpr.lockStrength); 18 has no such
 *		action.  A walker that tests for UPDATE and treats every other action as
 *		NOTHING would let DO SELECT through, so code that classifies a clause
 *		also tests ks_onconflict_action_shippable() on the action.
 */
#if PG_VERSION_NUM >= 190000
static inline bool
ks_onconflict_is_select(const OnConflictExpr *oc)
{
	return oc->action == ONCONFLICT_SELECT;
}
#else
static inline bool
ks_onconflict_is_select(const OnConflictExpr *oc)
{
	(void) oc;
	return false;
}
#endif
#define KS_ONCONFLICT_IS_SELECT(oc) ks_onconflict_is_select(oc)

/*
 * KS_IS_GROUPED_REL
 *		Is a RelOptInfo a grouped relation?  PostgreSQL 19 adds eager
 *		aggregation (enable_eager_aggregate): a grouped join relation has the
 *		same relids as the join relation it is made from, and
 *		set_join_pathlist_hook runs for it too.  A hook that adds a path for the
 *		join relation of a query returns at once for a grouped relation.  18 has
 *		no grouped relations.
 */
#if PG_VERSION_NUM >= 190000
static inline bool
ks_is_grouped_rel(const RelOptInfo *rel)
{
	return IS_GROUPED_REL(rel);
}
#else
static inline bool
ks_is_grouped_rel(const RelOptInfo *rel)
{
	(void) rel;
	return false;
}
#endif
#define KS_IS_GROUPED_REL(rel) ks_is_grouped_rel(rel)

/*
 * KS_PARTIAL_PLAN_GUCS
 *		The settings that a shard group applies while it plans a statement that
 *		has to return partial aggregate states: no parallel workers (a cursor
 *		cannot use them), no partitionwise aggregation, and, on 19, no eager
 *		aggregation (a partial Agg below a join would be a second partial
 *		aggregation of the same rows).  The list is a sequence of {name, value}
 *		pairs for an array of KsPlanSetting; both names are checked against a
 *		running server by t/098_foundation.pl.
 */
typedef struct KsPlanSetting
{
	const char *name;
	const char *value;
} KsPlanSetting;

#if PG_VERSION_NUM >= 190000
#define KS_PARTIAL_PLAN_GUCS \
	{"max_parallel_workers_per_gather", "0"}, \
	{"enable_partitionwise_aggregate", "off"}, \
	{"enable_eager_aggregate", "off"}
#else
#define KS_PARTIAL_PLAN_GUCS \
	{"max_parallel_workers_per_gather", "0"}, \
	{"enable_partitionwise_aggregate", "off"}
#endif

static const KsPlanSetting ks_partial_plan_gucs[] = {KS_PARTIAL_PLAN_GUCS};
#define KS_N_PARTIAL_PLAN_GUCS ((int) lengthof(ks_partial_plan_gucs))

/*
 * ks_result_adopt
 *		Give a libpq result another owner: the memory context ctx.  On 19 a
 *		result is wrapped in an object made in the memory context that was
 *		current when it was received, and freed when that context is reset
 *		(KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT); a result that has to outlive that
 *		context is moved to a context that lives long enough, and the pointer
 *		that is returned replaces the old one, which must not be used again.  On
 *		18 the code that got the result frees it, and nothing changes.
 */
#if PG_VERSION_NUM >= 190000
static inline PGresult *
ks_result_adopt(PGresult *res, MemoryContext ctx)
{
	return libpqsrv_PGresultSetParent(res, ctx);
}
#else
static inline PGresult *
ks_result_adopt(PGresult *res, MemoryContext ctx)
{
	(void) ctx;
	return res;
}
#endif

/*
 * ks_window_ignore_nulls
 *		The null treatment of a window function call, non-zero for IGNORE NULLS
 *		or RESPECT NULLS.  PostgreSQL 19 adds WindowFunc.ignore_nulls; 18 has no
 *		such clause.  A deparser that ships a window function refuses one whose
 *		value is not 0 until a test covers it.
 */
#if PG_VERSION_NUM >= 190000
static inline int
ks_window_ignore_nulls(const WindowFunc *wfunc)
{
	return wfunc->ignore_nulls;
}
#else
static inline int
ks_window_ignore_nulls(const WindowFunc *wfunc)
{
	(void) wfunc;
	return 0;
}
#endif

/*
 * KS_SET_PLAN_ORIGIN
 *		Say where a hand-made PlannedStmt comes from.  PostgreSQL 19 adds
 *		PlannedStmt.planOrigin (reporting data, for pg_stat_statements); a
 *		makeNode() leaves it at PLAN_STMT_UNKNOWN, so a plan built by hand needs
 *		no call, and this is the place to say otherwise if that changes.  On 18
 *		it does nothing.
 */
#if PG_VERSION_NUM >= 190000
#define KS_SET_PLAN_ORIGIN(pstmt, origin) ((pstmt)->planOrigin = (origin))
#define KS_PLAN_ORIGIN_UNKNOWN PLAN_STMT_UNKNOWN
#else
#define KS_SET_PLAN_ORIGIN(pstmt, origin) ((void) (origin))
#define KS_PLAN_ORIGIN_UNKNOWN 0
#endif

/*
 * KS_EAGER_AGGREGATE_GUC
 *		The setting that keeps a partial aggregation out of the joins of a plan.  PostgreSQL 19 has
 *		enable_eager_aggregate (on by default); the plan of the fragment of a partial aggregation must
 *		have one Agg node at its top (planner.c, partialize), so it is set to off there.  18 has no such
 *		setting, and the macro is not defined.
 */
#if PG_VERSION_NUM >= 190000
#define KS_EAGER_AGGREGATE_GUC "enable_eager_aggregate"
#endif

#endif							/* KINGRES_SHARD_PG_COMPAT_H */
