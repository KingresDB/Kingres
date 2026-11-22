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
#include "nodes/pg_list.h"

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
	KS_MERGE_ROUTED = 2			/* M3: the origin reads the source and routes each row to its group */
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
} KsMergeShip;

extern bool kingres_shard_merge_build_ship(Query *q, const KsMerge *m, KsMergeShip *sh, const char **why);

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

#endif							/* KINGRES_SHARD_MERGE_H */
