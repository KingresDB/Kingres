/*-------------------------------------------------------------------------
 *
 * pg_compat.h
 *	  The differences between the PostgreSQL majors that kingres_ledger
 *	  builds against (18 and 19), in one place.
 *
 * One source builds against every supported major (ADR 0023).  Code outside
 * this file does not test PG_VERSION_NUM: it uses what this file defines,
 * which is the PostgreSQL 19 spelling where a name changed, with a
 * definition for 18.  When a major is dropped, its branch of each #if goes
 * and the wrappers can be replaced by their one remaining body.
 *
 * Every definition says which upstream change it follows.  The header is
 * included by kingres_ledger.h, after postgres.h, so every file has it.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 * IDENTIFICATION
 *	  contrib/kingres_ledger/src/pg_compat.h
 *
 *-------------------------------------------------------------------------
 */
#ifndef KINGRES_LEDGER_PG_COMPAT_H
#define KINGRES_LEDGER_PG_COMPAT_H

#include "nodes/miscnodes.h"
#include "utils/numeric.h"

/*
 * pg_fallthrough marks a deliberate fall through from one case to the next.
 *
 * PostgreSQL 19 builds with -Wimplicit-fallthrough=5, which no longer
 * accepts the comment form of the marker, and provides pg_fallthrough in
 * c.h (release 19, "Use standard C23 and C++ attributes if available").
 * Extensions built with PGXS on 19 get the same flag.  On 18 the macro does
 * not exist.
 */
#ifndef pg_fallthrough
#if __has_attribute(fallthrough)
#define pg_fallthrough __attribute__((fallthrough))
#else
#define pg_fallthrough
#endif
#endif

/*
 * kl_numeric_to_int8
 *		Convert a numeric to int8 without raising an error: when the value is
 *		out of range, *have_error is set and the result is not meaningful.
 *		Rounds, like the cast does.  The caller sets *have_error to false
 *		first.
 *
 * PostgreSQL 19 replaced numeric_int8_opt_error(num, &have_error) with
 * numeric_int8_safe(num, escontext), which reports through an error saving
 * context.
 */
#if PG_VERSION_NUM >= 190000
static inline int64
kl_numeric_to_int8(Numeric num, bool *have_error)
{
	ErrorSaveContext escontext = {T_ErrorSaveContext};
	int64		result;

	result = numeric_int8_safe(num, (Node *) &escontext);
	if (escontext.error_occurred)
		*have_error = true;
	return result;
}
#else
#define kl_numeric_to_int8(num, have_error) \
	numeric_int8_opt_error((num), (have_error))
#endif

#endif							/* KINGRES_LEDGER_PG_COMPAT_H */
