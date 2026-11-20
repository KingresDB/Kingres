/*-------------------------------------------------------------------------
 *
 * catalog.h
 *	  The per-backend cache of the kingres catalog tables and the lookups
 *	  built on it.
 *
 * SPDX-License-Identifier: AGPL-3.0-only
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
	bool		is_schema;		/* a table of a distributed schema (phase 11): one bucket, no key; kept in schema_tables, never in dist_tables */
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
	int64		term;			/* catalog term: raised by every promotion (failover.c) */
	int64		superseded_by_term; /* 0, or the newer term this node learned of while acting as the map primary */
	int			local_node_id;	/* 0 when the cluster is not initialized */
	int			local_timeline; /* WAL timeline recorded when this node became what it is; 0: not recorded (S9-02) */
	int			local_group_id;	/* -1 when the cluster is not initialized */
	int			ngroups;		/* number of groups with an active primary */
	int			ndist_tables;
	List	   *nodes;			/* of KsNode * */
	List	   *colocations;	/* of KsColocation * */
	HTAB	   *dist_tables;	/* relid -> KsDistTable */
	int			nschema_tables;
	HTAB	   *schema_tables;	/* relid -> KsDistTable (is_schema) of the tables of distributed schemas; no code that
								 * works on hash-distributed tables sees them (phase 11, design/shard-phase11.md 5.1) */
} KsCatalog;

extern void kingres_shard_catalog_init(void);
extern const KsCatalog *kingres_shard_catalog(void);
extern bool kingres_shard_cluster_secret(unsigned char *buf, int bufsize, int *len);
extern bool kingres_shard_previous_secret(unsigned char *buf, int bufsize, int *len);
extern bool kingres_shard_cluster_secret_for_names(unsigned char *buf, int bufsize, int *len);
extern int kingres_shard_local_node_id_raw(void);
extern void kingres_shard_catalog_invalidate(void);
extern int64 kingres_shard_catalog_generation(void);
extern KsColocation *kingres_shard_coloc_copy(const KsColocation *coloc);
extern KsColocation *kingres_shard_coloc_make(int colocation_id, int nbuckets, Oid dist_type, Oid collation,
											  const int *groups);
extern int64 kingres_shard_placement_hash(const KsColocation *coloc);
extern int64 kingres_shard_placement_hash_now(int colocation_id);
extern int64 kingres_shard_map_version_now(void);
extern int64 kingres_shard_term_now(void);
extern int64 kingres_shard_partition_set_identity(int n, const Oid *oids);
extern int64 kingres_shard_partition_identity_now(Oid parent);
extern const KsDistTable *kingres_shard_lookup_dist_table(Oid relid);
extern const KsColocation *kingres_shard_lookup_colocation(int colocation_id);

/*
 * Tables of distributed schemas (phase 11).  A schema table is a table of one colocation group of kind
 * schema: one bucket, bucket 0, held by one shard group, the owner; on every other group the table is an
 * empty "shell" that must never be read as if it held the rows.  The lookup returns NULL for a relation
 * that is not one.
 */
extern const KsDistTable *kingres_shard_lookup_schema_table(const KsCatalog *cat, Oid relid);
extern int	kingres_shard_schema_table_owner(const KsDistTable *st);
extern const KsDistTable *kingres_shard_lookup_scan_table(const KsCatalog *cat, Oid relid);

/*
 * The buckets of a colocation group.  Code that walks the buckets, or asks
 * which group owns one, uses these and never reads bucket_count or
 * bucket_group itself, and never assumes that the numbers of the buckets are
 * 0 .. n - 1 (design/shard-phase2b.md, section 4.7; hashing.h has the lookup
 * from a hash value).  The i-th bucket, for i in 0 .. nbuckets - 1, is
 * kingres_shard_coloc_bucket(); the group that owns a bucket is -1 when the
 * bucket is not mapped or is not a bucket of the group.
 */
extern int	kingres_shard_coloc_nbuckets(const KsColocation *coloc);
extern int	kingres_shard_coloc_bucket(const KsColocation *coloc, int i);
extern int	kingres_shard_bucket_modulus(const KsColocation *coloc, int bucket);
extern int	kingres_shard_bucket_group(const KsColocation *coloc, int bucket);
extern const KsNode *kingres_shard_group_primary(int group_id);
extern const KsNode *kingres_shard_node_by_id(int node_id);
extern Oid	kingres_shard_catalog_relid(const char *relname);

#endif							/* KINGRES_SHARD_CATALOG_H */
