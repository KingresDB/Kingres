# contrib/kingres_sketch/Makefile
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

MODULE_big = kingres_sketch
OBJS = \
	$(WIN32RES) \
	src/hll.o \
	src/kingres_sketch.o \
	src/sketch_hash.o \
	src/tdigest.o \
	src/topn.o

EXTENSION = kingres_sketch
DATA = kingres_sketch--1.0.sql
PGFILEDESC = "kingres_sketch - approximate aggregates (hll, topn, tdigest) for KingresDB"

REGRESS = sketch_setup sketch_hash sketch_hll sketch_topn sketch_tdigest sketch_io sketch_security

TAP_TESTS = 1

PG_CPPFLAGS = -I$(srcdir)/src -Werror=implicit-function-declaration

ifdef USE_PGXS
PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
else
subdir = contrib/kingres_sketch
top_builddir = ../..
include $(top_builddir)/src/Makefile.global
include $(top_srcdir)/contrib/contrib-global.mk
endif
