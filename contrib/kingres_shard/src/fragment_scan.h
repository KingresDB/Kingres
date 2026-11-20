/*-------------------------------------------------------------------------
 *
 * fragment_scan.h
 *	  The fragments of a read that is run by the shard groups: the machinery
 *	  that starts, reads, drains and stops one cursor per group (shared by the
 *	  "Kingres Distributed Scan" of dist_scan.c and the "Kingres Fragment Scan"
 *	  of this file), and the Fragment Scan itself, which runs a closed query
 *	  whole on every group (design/shard-phase2b.md, 4.4 and 6.4).
 *
 * SPDX-License-Identifier: AGPL-3.0-only
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/fragment_scan.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_FRAGMENT_SCAN_H
#define KINGRES_SHARD_FRAGMENT_SCAN_H

#include "postgres.h"

#include "datatype/timestamp.h"
#include "nodes/params.h"
#include "nodes/parsenodes.h"
#include "nodes/pg_list.h"

#include "catalog.h"
#include "node_conn.h"

/* Rows requested from a group at a time (the setting of both scans, defined in dist_scan.c). */
extern PGDLLIMPORT int kingres_shard_scan_fetch_size;

/* ------------------------------------------------------------------------
 * The machinery of a set of fragments (moved out of dist_scan.c)
 * ---------------------------------------------------------------------- */

struct KsFragSet;

/* A fragment: the query of one remote group, read through a cursor. */
typedef struct KsFrag
{
	struct KsFragSet *set;
	KsConn	   *kc;
	char		cursor[40];
	bool		declared;
	bool		exhausted;
	bool		in_flight;		/* a FETCH was sent and not yet read */
	bool		failed;			/* the answer was read where no error may be raised: raise it when the batch is needed */
	PGresult   *res;			/* the batch being consumed */
	int			nrows;
	int			next_row;
	char	   *host;
	int			port;
	int			group_id;
	int64	   *identity;		/* identity of the node's partitions of every table of the probe when first asked */
	TimestampTz sent_at;		/* when the first FETCH was sent (EXPLAIN ANALYZE) */
	double		first_ms;		/* milliseconds from that to the first batch */
	bool		first_seen;
} KsFrag;

/* The fragments of one scan, and what they need to be read in turn. */
typedef struct KsFragSet
{
	MemoryContext qcxt;			/* the context a libpq result is made in: it lives as long as the scan */
	List	   *frags;			/* of KsFrag * */
	int			cur;			/* round-robin position */
	int			ntables;		/* tables of the probe: the length of every identity array */
	const char *fp_prefix;		/* "scan_" or "frag_": the prefix of the fault points of this scan */
	bool		started;
} KsFragSet;

/* One item of a pruning specification: the pruning terms of one distributed table (prune.c). */
typedef struct KsFragPrune
{
	Oid			relid;
	List	   *terms;
} KsFragPrune;

/*
 * What kingres_shard_fragset_start() needs.  The procedure is the placement check of design 4.3
 * (placement_check.c): the probe before, the DECLAREs, the probe after, and no FETCH before every
 * node has passed.  The origin's own part has two places in it: local_open runs after the
 * DECLAREs were answered and before the probe after; after_check runs after the probe after.
 */
typedef struct KsFragStart
{
	int			colocation_id;
	int			ntables;
	const Oid  *relids;			/* the tables of the probes */
	int64		plan_placement;
	const char *marker;			/* the comment that starts the statement of the DECLARE */
	const char *ext;			/* "name:version,..." of the extensions the statement uses, or NULL (the probe asks the node for them) */
	const char *sql;			/* the query of the cursor */
	int			nparams;
	const Oid  *ptypes;
	const char *const *pvalues;
	/* J1: the values of the parameters of one group (the arrays of the shipped relation differ), NULL: pvalues for all */
	const char *const *(*group_values) (void *arg, int group_id);
	void		(*local_open) (void *arg);
	void		(*after_check) (void *arg);
	void	   *arg;
	/* fault points: after the probe before, after the DECLAREs, after the probe after; "<prefix>declared_g<n>" per group */
	const char *fp_after_probe_before;
	const char *fp_after_declare;
	const char *fp_after_check;
	const char *fp_first_fetch;	/* NULL: none */
	const char *fp_declared_prefix;
} KsFragStart;

/*
 * The groups a read of the tables of a colocation group has to ask, as a list of KsFrag (allocated
 * in the current context, to be put in KsFragSet.frags): the primaries of the groups that own a
 * bucket and that every pruning item allows (design 4.5).  The group of this node is not in the
 * list; *local says whether it is one of the groups to ask (it is not when this node is not the
 * primary of its group).  *nall is the number of groups before pruning, *pruned whether an item applied.
 */
extern List *kingres_shard_frag_choose(const KsColocation *coloc, const KsCatalog *cat, List *prune,
									   ParamListInfo params, bool *local, int *nall, bool *pruned);

/* The batches of a scan that an error ended are libpq memory: a reset callback of qcxt clears them. */
extern void kingres_shard_fragset_init(KsFragSet *fs, MemoryContext qcxt, int ntables);

extern void kingres_shard_fragset_start(KsFragSet *fs, const KsFragStart *st);

/*
 * The next row of the remote groups: false at the end, else *res and *row say where it is (the
 * batch is valid until the next call).  Batches are taken in turn and the next batch of a group is
 * requested as soon as the current one is used up.
 */
extern bool kingres_shard_fragset_next(KsFragSet *fs, PGresult **res, int *row, KsFrag **frag);

/*
 * The ordered merge (design 8): every group is a stream of its own.  _prime sends the first FETCH to every group at once; _next_of
 * gives the next row of one group (the batch is valid until the next call for that group), false at its end.
 */
extern void kingres_shard_fragset_prime(KsFragSet *fs);
extern bool kingres_shard_fragset_next_of(KsFragSet *fs, KsFrag *f, PGresult **res, int *row);

/* Close the cursors (when asked), read what is in flight, free the fragments. */
extern void kingres_shard_fragset_stop(KsFragSet *fs, bool close_cursors);

/*
 * A fragment carries only the external parameters it uses, numbered from $1: the mutator replaces
 * every external Param by one with the next number and records the original id in ids (position =
 * new id - 1).
 */
typedef struct KsParamRenumber
{
	List	   *ids;			/* of int */
} KsParamRenumber;

extern Node *kingres_shard_renumber_params(Node *node, KsParamRenumber *ctx);

/* The value of the external parameters of a fragment as text, for the ids in order. */
extern const char **kingres_shard_fragment_params(ParamListInfo params, List *param_ids,
												  Oid **types_out, int *n_out);

/* ------------------------------------------------------------------------
 * The Fragment Scan (closed queries)
 * ---------------------------------------------------------------------- */

/*
 * What planner_hook_body hands over to the paths of the statement it plans (design 6, "PushdownInfo
 * hand-over"): the query that was decided, the text every group runs and what the plan needs to
 * check when it starts.
 */
typedef struct KsPushdownInfo
{
	Query	   *parse;			/* the query of the planner call: root->parse == parse recognises it */
	char	   *sql;			/* the statement every group runs, parameters renumbered from $1 */
	List	   *param_ids;		/* of int: the external parameter id of $1, $2, ... */
	int			colocation_id;
	int64		place;			/* the placement number of the colocation group the statement was decided with */
	List	   *relids;			/* of Oid: every relation of the statement (distributed and reference) */
	List	   *dist_relids;	/* of Oid: the distributed tables, which the probes name */
	const char *kind;			/* what is pushed, for EXPLAIN (a static string) */
	int			ncols;			/* the columns the groups return */
	char	   *ext;			/* "name:version,..." of the extensions of the text (design 5.5), or NULL */
	List	   *sort;			/* step 9: of List(column, sort operator, collation, nulls first): the groups sort, this node merges */
	bool		limit_pushed;	/* LIMIT count + offset is in the text */
	int			ndist;			/* the references to distributed tables (a self join has two of one table) */
	Node	   *limit_cnt;		/* step 9: a LIMIT that is not a constant: the expressions, computed when the scan starts and sent as one parameter */
	Node	   *limit_off;
	int			limit_param;	/* the number of that parameter, 0 for none */
	bool		closed;			/* a closed query: the whole statement is one fragment */
	bool		join;			/* step 10: the join of the level may be a fragment of its own (rows of the join) */
	void	   *join_done;		/* the join relation that has its fragment path */
	bool		partial;		/* a partial aggregation (design 7): sql and ncols are made by the path, which knows the partial target */
	Query	   *ship;			/* partial: a copy of the query, to deparse with the select list of the partial target */
	List	   *repart;			/* J1: of KsRepartCand *: the relations that can be shipped (repartition.h); join is also set */
} KsPushdownInfo;

extern void kingres_shard_fragment_scan_init(void);

/* The info the paths of the statement being planned may use, or NULL.  Returns the previous one. */
extern KsPushdownInfo *kingres_shard_pushdown_info_set(KsPushdownInfo *info);

/*
 * Why the statement being planned is not a pushed one, for EXPLAIN VERBOSE of the scans it gets
 * instead (NULL: the question was not asked, a setting is off).  _swap sets it and returns the old one.
 */
extern const char *kingres_shard_pushdown_reason_swap(const char *reason);
extern const char *kingres_shard_pushdown_reason(void);

#endif							/* KINGRES_SHARD_FRAGMENT_SCAN_H */
