# contrib/kingres_shard/Makefile
#
# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The libpq handling follows contrib/postgres_fdw/Makefile.

MODULE_big = kingres_shard
OBJS = \
	$(WIN32RES) \
	src/bucket_move.o \
	src/catalog.o \
	src/dist_scan.o \
	src/dist_table.o \
	src/fanout.o \
	src/failover.o \
	src/fault.o \
	src/forward_auth.o \
	src/ha.o \
	src/hashing.o \
	src/kingres_shard.o \
	src/node_admin.o \
	src/node_conn.o \
	src/placement_check.o \
	src/planner.o \
	src/prune.o \
	src/recovery.o \
	src/remote_scan.o \
	src/shippable.o \
	src/term.o \
	src/utility.o
PGFILEDESC = "kingres_shard - KingresDB sharding extension"

# A call of a function that no header declares must stop the build, not print a
# warning that nobody reads (a missing prototype once went unnoticed).
PG_CPPFLAGS = -I$(libpq_srcdir) -I$(srcdir)/src -Werror=implicit-function-declaration
SHLIB_LINK_INTERNAL = $(libpq)

EXTENSION = kingres_shard
DATA = kingres_shard--0.1.sql kingres_shard--0.2.sql kingres_shard--0.3.sql \
	kingres_shard--0.4.sql kingres_shard--0.1--0.2.sql kingres_shard--0.2--0.3.sql \
	kingres_shard--0.3--0.4.sql kingres_shard--0.5.sql kingres_shard--0.4--0.5.sql

REGRESS = version catalog bucket_for bucket_lookup distribute routing_single security \
	utility_guard routing_decisions reference ddl_single rebalance_plan upgrade
# The tests need the extension preloaded (see kingres_shard.conf).
REGRESS_OPTS = --temp-config $(srcdir)/kingres_shard.conf
TAP_TESTS = 1

ifdef USE_PGXS
PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
else
SHLIB_PREREQS = submake-libpq
subdir = contrib/kingres_shard
top_builddir = ../..
include $(top_builddir)/src/Makefile.global
include $(top_srcdir)/contrib/contrib-global.mk
endif
