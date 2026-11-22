/*-------------------------------------------------------------------------
 *
 * merge.h
 *	  MERGE on distributed tables (design/analytics-phase12-queries.md, section 7).
 *
 * The decision is made on the rewritten MERGE statement, by the origin on its own tree and by a
 * receiving group on the parse of the text it received: both run the same function
 * (kingres_shard_merge_fragment_analyze), so that a statement that the origin ships is never
 * refused by a group, and a text that is not of an accepted shape is never run.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/merge.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_MERGE_H
#define KINGRES_SHARD_MERGE_H

#include "postgres.h"

#include "access/attnum.h"
#include "nodes/parsenodes.h"
#include "nodes/params.h"
#include "nodes/pg_list.h"
#include "utils/memutils.h"

/* kingres_shard.enable_distributed_merge: off gives the refusals of phase 2a */
extern PGDLLIMPORT bool kingres_shard_enable_distributed_merge;
/* kingres_shard.merge_max_source_bytes (kB): the rows of a MERGE source that the origin holds */
extern PGDLLIMPORT int kingres_shard_merge_max_source_kb;

extern void kingres_shard_merge_init(void);

/* How a MERGE into a distributed table is run. */
typedef enum KsMergeKind
{
	KS_MERGE_NONE = 0,
	KS_MERGE_COLOCATED = 1,		/* M1: the whole statement runs on every group of the target */
	KS_MERGE_ROUTED = 2,		/* M3: the origin reads the source and routes each row to its group */
	KS_MERGE_REFERENCE = 3,		/* M4: a reference table is the target, the statement runs on every node */
	KS_MERGE_REFERENCE_SHIPPED = 4	/* M5: the same, the source is read here and every node gets all of it */
} KsMergeKind;

typedef struct KsMerge
{
	KsMergeKind kind;
	Oid			target_relid;
	Index		target_rti;
	int			colocation_id;
	AttrNumber	dist_attno;
	Oid			dist_type;
	bool		has_nmbs;		/* a WHEN NOT MATCHED BY SOURCE action: no group is skipped */
	bool		has_insert;		/* a WHEN NOT MATCHED [BY TARGET] ... INSERT action */
	List	   *relids;			/* every relation of the statement, target included */
	/* M3 */
	Index		source_rti;		/* the one relation of USING */
	Node	   *route;			/* the expression of the source that the ON clause equates with the key */
} KsMerge;

/* The shipped form of an M3 statement (design 3.1), and the SELECT that reads its source. */
typedef struct KsMergeShip
{
	Query	   *shipped;		/* the statement with the source replaced by the sub-select over unnest() */
	char	   *text;			/* its deparsed text, without a marker */
	char	   *source_sql;		/* the SELECT the origin runs: the shipped columns, then the route expression */
	int			nparams_ext;	/* external parameters of the statement */
	int			first_param;	/* number of the first array parameter */
	int			ncols;			/* arrays: columns shipped */
	List	   *attnos;			/* the attribute numbers of the source that are shipped, in array order */
	Oid			route_type;
	bool		broadcast;		/* no route: every node gets every row (M5) */
} KsMergeShip;

extern bool kingres_shard_merge_build_ship(Query *q, const KsMerge *m, KsMergeShip *sh, const char **why);

/* The marker that starts the text of a MERGE fragment: the placement number it was decided with, and
 * the number of shipped sub-selects (0: colocated, 1: a routed source). */
#define KS_MERGE_MARKER_PREFIX "/* kingres:merge-fragment "
extern char *kingres_shard_merge_marker(int64 place, int rp);
extern bool kingres_shard_parse_merge_marker(const char *query_string, int64 *place, int *rp);

/* The text of an M1 statement as the groups get it (without the marker); NULL with *why when a group would not accept it. */
extern char *kingres_shard_merge_text_colocated(Query *parse, const char **why);

/* M3: the rows of the source of a MERGE, kept by the group that owns the bucket of their key. */
typedef struct KsMergeShares KsMergeShares;

extern KsMergeShares *kingres_shard_merge_collect(const char *source_sql, ParamListInfo params,
												  int ncols, int colocation_id, List *group_ids,
												  MemoryContext cxt);
extern const char *kingres_shard_merge_share_array(KsMergeShares *sh, int group_id, int col);
extern int64 kingres_shard_merge_share_rows(KsMergeShares *sh, int group_id);

/*
 * Decide a MERGE whose target is a distributed table.  True with *m filled when the statement is
 * of a shape that runs (M1 or M3).  False when the target is not a distributed table of this
 * cluster (not this file's business).  A statement of this target that no shape accepts is
 * refused here with an error that names the missing condition, before any group has run anything.
 */
extern bool kingres_shard_merge_classify(Query *parse, KsMerge *m);

/*
 * The decision of a statement as it is shipped (rp = the number of shipped sub-selects the marker
 * says): the receiving group runs this on the parse of the text, the origin on the parse of the
 * text it deparsed.  False with *why (a static or palloc'd string) when it is not of an accepted
 * shape.
 */
extern bool kingres_shard_merge_fragment_analyze(Query *q, int rp, KsMerge *m, const char **why);

/* M4 and M5: a MERGE into a reference table.  The analysis a node makes of the text of a reference-table write; *nshipped the shipped sub-selects. */
extern bool kingres_shard_merge_ref_fragment_analyze(Query *q, int *nshipped, const char **why);
extern bool kingres_shard_merge_classify_ref(Query *parse, KsMerge *m);
extern char *kingres_shard_merge_text_reference(Query *parse, const char **why);

/* M6: fence the distributed tables of USING of a MERGE into a local table; the number fenced. */
extern int	kingres_shard_merge_fence_sources(Query *parse);

#endif							/* KINGRES_SHARD_MERGE_H */
