/*-------------------------------------------------------------------------
 *
 * pg_compat.h
 *	  The only place of the extension that knows which PostgreSQL major
 *	  version it is built for.
 *
 * One source tree builds against PostgreSQL 18 and 19 (ADR 0023): no
 * other file tests PG_VERSION_NUM.  A difference between the majors is
 * absorbed here, by a macro or an inline function with the name the rest
 * of the code uses.  At present the APIs the extension uses are the same
 * in 18 and 19, so the file only fixes the module magic.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_sketch/src/pg_compat.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_SKETCH_PG_COMPAT_H
#define KINGRES_SKETCH_PG_COMPAT_H

/* Module magic with a name and a version (available since PostgreSQL 18). */
#define KS_MODULE_MAGIC() \
	PG_MODULE_MAGIC_EXT(.name = "kingres_sketch", .version = "1.0")

#endif							/* KINGRES_SKETCH_PG_COMPAT_H */
