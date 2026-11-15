# contrib/kingres_columnar/Makefile
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

MODULE_big = kingres_columnar
OBJS = \
	$(WIN32RES) \
	src/kc_am.o \
	src/kc_catalog.o \
	src/kc_cscan.o \
	src/kc_encode.o \
	src/kc_scan.o \
	src/kc_skip.o \
	src/kc_storage.o \
	src/kc_vacuum.o \
	src/kc_write.o \
	src/kingres_columnar.o

EXTENSION = kingres_columnar
DATA = kingres_columnar--0.1.sql
PGFILEDESC = "kingres_columnar - columnar table access method for KingresDB"

REGRESS = columnar_setup columnar_basic columnar_types columnar_ddl \
	columnar_refusals columnar_scan_plans columnar_stats columnar_vacuum \
	columnar_analyze columnar_security

TAP_TESTS = 1

PG_CPPFLAGS = -I$(srcdir)/src -Werror=implicit-function-declaration

# Link the compression libraries the server itself was built with: the codecs
# of the extension follow the USE_LZ4 and USE_ZSTD of its pg_config.h.
ifdef USE_PGXS
PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
kc_pg_config_h := $(shell $(PG_CONFIG) --includedir-server)/pg_config.h
else
kc_pg_config_h := $(top_builddir)/src/include/pg_config.h
endif
ifneq ($(shell grep -c '^\#define USE_LZ4 1' $(kc_pg_config_h) 2>/dev/null),0)
SHLIB_LINK += -llz4
endif
ifneq ($(shell grep -c '^\#define USE_ZSTD 1' $(kc_pg_config_h) 2>/dev/null),0)
SHLIB_LINK += -lzstd
endif

ifdef USE_PGXS
include $(PGXS)
else
subdir = contrib/kingres_columnar
top_builddir = ../..
include $(top_builddir)/src/Makefile.global
include $(top_srcdir)/contrib/contrib-global.mk
endif
