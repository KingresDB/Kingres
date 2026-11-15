# contrib/kingres_timepart/Makefile
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

EXTENSION = kingres_timepart
DATA = kingres_timepart--0.1.sql
PGFILEDESC = "kingres_timepart - time partitioning for KingresDB"

REGRESS = timepart_setup timepart_grid timepart_create timepart_policy timepart_run timepart_retention timepart_names timepart_hostile timepart_security timepart_drop
REGRESS_OPTS = --temp-config $(srcdir)/kingres_timepart.conf

TAP_TESTS = 1

ifdef USE_PGXS
PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
else
subdir = contrib/kingres_timepart
top_builddir = ../..
include $(top_builddir)/src/Makefile.global
include $(top_srcdir)/contrib/contrib-global.mk
endif
