# contrib/kingres_timepart/Makefile
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

EXTENSION = kingres_timepart
DATA = kingres_timepart--0.1.sql kingres_timepart--0.1--0.2.sql kingres_timepart--0.2.sql
PGFILEDESC = "kingres_timepart - time partitioning for KingresDB"

REGRESS = timepart_setup timepart_grid timepart_create timepart_policy timepart_run timepart_retention timepart_names timepart_hostile timepart_security timepart_history timepart_privilege timepart_forged timepart_owner_code timepart_convert timepart_drop upgrade
REGRESS_OPTS = --temp-config $(srcdir)/kingres_timepart.conf

TAP_TESTS = 1

ifdef USE_PGXS
PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
else
# The tests convert partitions to columnar storage.
EXTRA_INSTALL = contrib/kingres_columnar
subdir = contrib/kingres_timepart
top_builddir = ../..
include $(top_builddir)/src/Makefile.global
include $(top_srcdir)/contrib/contrib-global.mk
endif
