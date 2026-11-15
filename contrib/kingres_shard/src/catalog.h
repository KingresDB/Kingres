/*-------------------------------------------------------------------------
 *
 * catalog.h
 *	  The per-backend cache of the kingres catalog tables and the lookups
 *	  built on it.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/catalog.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SHARD_CATALOG_H
#define KINGRES_SHARD_CATALOG_H

#include "postgres.h"

#include "access/attnum.h"
#include "nodes/pg_list.h"
#include "utils/hsearch.h"
#include "utils/palloc.h"

/* A colocation group: bucket count, key type and the bucket placement. */
typedef struct KsColocation
{
	int			colocation_id;
	int			bucket_count;
	Oid			dist_type;
	Oid			collation;
	int		   *bucket_group;	/* bucket_count entries; -1 if unmapped */
} KsColocation;

/* A distributed table, as cached.  The hash key is relid. */
typedef struct KsDistTable
{
	Oid			relid;
	AttrNumber	dist_attno;
	int			colocation_id;
	KsColocation *coloc;
	NameData	dist_column;
	bool		is_reference;	/* a reference table: no key, no colocation group */
} KsDistTable;

/* A node of the cluster. */
typedef struct KsNode
{
	int			node_id;
	int			group_id;
	char	   *host;
	int			port;
	bool		is_primary;
	bool		is_active;
	bool		should_have_shards;
} KsNode;

/*
 * The cached catalog.  "present" is false when the extension is not
 * installed in this database.  The structure and everything it points to
 * stay valid only until the next call of kingres_shard_catalog() after an
 * invalidation: callers copy what they need to keep.
 */
typedef struct KsCatalog
{
	bool		present;
	int64		map_version;
	int			local_node_id;	/* 0 when the cluster is not initialized */
	int			local_group_id;	/* -1 when the cluster is not initialized */
	int			ngroups;		/* number of groups with an active primary */
	int			ndist_tables;
	List	   *nodes;			/* of KsNode * */
	List	   *colocations;	/* of KsColocation * */
	HTAB	   *dist_tables;	/* relid -> KsDistTable */
} KsCatalog;

extern void kingres_shard_catalog_init(void);
extern const KsCatalog *kingres_shard_catalog(void);
extern bool kingres_shard_cluster_secret(unsigned char *buf, int bufsize, int *len);
extern int kingres_shard_local_node_id_raw(void);
extern void kingres_shard_catalog_invalidate(void);
extern int64 kingres_shard_placement_hash(const KsColocation *coloc);
extern int64 kingres_shard_placement_hash_now(int colocation_id);
extern int64 kingres_shard_map_version_now(void);
extern int64 kingres_shard_partition_set_identity(int n, const Oid *oids);
extern int64 kingres_shard_partition_identity_now(Oid parent);
extern const KsDistTable *kingres_shard_lookup_dist_table(Oid relid);
extern const KsColocation *kingres_shard_lookup_colocation(int colocation_id);
extern const KsNode *kingres_shard_group_primary(int group_id);
extern const KsNode *kingres_shard_node_by_id(int node_id);
extern Oid	kingres_shard_catalog_relid(const char *relname);

#endif							/* KINGRES_SHARD_CATALOG_H */
