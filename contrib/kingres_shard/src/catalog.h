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

/*
 * A colocation group: key type and the layout of its buckets.
 *
 * A bucket is a hash partition (MODULUS m, REMAINDER r); its number is its remainder r.
 * A group that was never split has bucket_count buckets 0 .. bucket_count - 1, all of
 * modulus bucket_count.  After a bucket is split (design/shard-phase10.md) the buckets have
 * different moduli, every modulus divides max_modulus, and the buckets together cover each
 * slot 0 .. max_modulus - 1 of hash % max_modulus exactly once.  The arrays below are
 * indexed by bucket number or by slot and have max_modulus entries; only the accessors
 * further down (and the code that builds a layout) touch them.
 */
typedef struct KsColocation
{
	int			colocation_id;
	int			bucket_count;	/* the base modulus a new table of the group asks for; NOT the number of buckets */
	int64		placement_epoch;	/* counts the switches of the group (move or split), see kingres_shard_placement_hash() */
	Oid			dist_type;
	Oid			collation;
	int			max_modulus;	/* the greatest modulus of a bucket; the size of the arrays */
	int			nbucket;		/* the number of buckets */
	int		   *bucket_list;	/* nbucket entries: the bucket numbers, ascending */
	int		   *bucket_group;	/* by bucket number: the owning group; -1 if unmapped or not a bucket */
	int		   *bucket_mod;		/* by bucket number: its modulus; 0 if not a bucket */
	int		   *slot_bucket;	/* by slot (hash % max_modulus): the bucket that holds it; -1 if a gap */
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

/*
 * Making a layout from the catalog tables as the caller's own transaction sees them (the
 * cache cannot be used for a group made in this transaction): begin, one add per row of
 * bucket_map, finish.  A row that cannot belong to the layout (modulus that does not divide
 * max_modulus, remainder not below the modulus) is left out, and the group then has a gap that
 * kingres.check_cluster() reports as bucket_layout.  All in the current memory context.
 */
extern KsColocation *kingres_shard_coloc_begin(int colocation_id, int bucket_count, int max_modulus,
											   int64 placement_epoch, Oid dist_type, Oid collation);
extern void kingres_shard_coloc_add(KsColocation *coloc, int bucket, int modulus, int group);
extern void kingres_shard_coloc_finish(KsColocation *coloc);
extern int64 kingres_shard_placement_hash(const KsColocation *coloc);
extern int64 kingres_shard_placement_hash_now(int colocation_id);
extern int64 kingres_shard_map_version_now(void);
extern int64 kingres_shard_term_now(void);
extern int64 kingres_shard_partition_set_identity(int n, const Oid *oids);
extern int64 kingres_shard_partition_identity_now(Oid parent);
extern const KsDistTable *kingres_shard_lookup_dist_table(Oid relid);
extern const KsColocation *kingres_shard_lookup_colocation(int colocation_id);

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
extern int	kingres_shard_coloc_bucket_of_hash(const KsColocation *coloc, uint64 hash);
extern const KsNode *kingres_shard_group_primary(int group_id);
extern const KsNode *kingres_shard_node_by_id(int node_id);
extern Oid	kingres_shard_catalog_relid(const char *relname);

#endif							/* KINGRES_SHARD_CATALOG_H */
