# contrib/kingres_ledger/Makefile
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

MODULE_big = kingres_ledger
OBJS = \
	$(WIN32RES) \
	src/capture.o \
	src/kingres_ledger.o \
	src/merkle.o \
	src/proof.o \
	src/protect.o \
	src/row_hash.o \
	src/worker.o

EXTENSION = kingres_ledger
DATA = kingres_ledger--0.1.sql kingres_ledger--0.2.sql kingres_ledger--0.1--0.2.sql
PGFILEDESC = "kingres_ledger - tamper-evident ledger tables for KingresDB"

REGRESS = ledger_setup ledger_hash ledger_capture ledger_verify ledger_protect ledger_format ledger_review ledger_updatable ledger_records ledger_privs
REGRESS_OPTS = --temp-config $(srcdir)/kingres_ledger.conf

TAP_TESTS = 1

PG_CPPFLAGS = -I$(srcdir)/src

ifdef USE_PGXS
PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
else
subdir = contrib/kingres_ledger
top_builddir = ../..
include $(top_builddir)/src/Makefile.global
include $(top_srcdir)/contrib/contrib-global.mk
endif
