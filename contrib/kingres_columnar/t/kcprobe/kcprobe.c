/*-------------------------------------------------------------------------
 *
 * kcprobe.c
 *	  A module for tests of kingres_columnar only.  It runs SQL statements
 *	  from callbacks of its own, at the two places where code of another
 *	  module reaches the access method after the access method has done its
 *	  own work:
 *
 *	  - the end of every subtransaction, in the window between the two phases
 *	    of the commit that the access method uses to hand the rows of a
 *	    subtransaction to its parent (setting kcprobe.sql);
 *	  - the pre-commit and the pre-prepare of the top-level transaction, where
 *	    the access method writes every buffer (setting kcprobe.commit_sql).
 *
 * Callbacks run last-registered first.  Loaded before kingres_columnar in
 * shared_preload_libraries, this module registers its callbacks first, so
 * they run after the access method's callbacks: the statement sees the
 * buffers as the access method left them.  Loaded after it, they run before
 * (the access method then still has its flush to come).
 *
 * The setting kcprobe.sql is the statement of the subtransaction callback;
 * its first result value is reported as a NOTICE.  A statement that begins
 * with "cursor:" is opened as a cursor named kcc that outlives the callback,
 * and one row is fetched.  The setting kcprobe.commit_sql is run at
 * XACT_EVENT_PRE_COMMIT and XACT_EVENT_PRE_PREPARE, and its first result
 * value is reported as a NOTICE too.  Not installed, never part of a
 * release.
 *
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 * SPDX-License-Identifier: AGPL-3.0-only
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include "access/xact.h"
#include "executor/spi.h"
#include "fmgr.h"
#include "utils/guc.h"
#include "utils/portal.h"
#include "utils/snapmgr.h"

PG_MODULE_MAGIC;

static char *probe_sql = NULL;
static char *probe_commit_sql = NULL;

static void
probe_run(const char *sql)
{
	bool		pushed = false;

	/* RELEASE and the pre-commit have no snapshot of their own, and SPI wants one */
	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}
	SPI_connect();
	if (strncmp(sql, "cursor:", 7) == 0)
	{
		/* a cursor named kcc that outlives the callback, first row fetched */
		SPIPlanPtr	plan = SPI_prepare(sql + 7, 0, NULL);
		Portal		portal;

		if (plan == NULL || SPI_keepplan(plan) != 0)
			elog(ERROR, "kcprobe: could not prepare the cursor");
		portal = SPI_cursor_open("kcc", plan, NULL, NULL, true);
		SPI_cursor_fetch(portal, true, 1);
		SPI_finish();
		if (pushed)
			PopActiveSnapshot();
		return;
	}
	if (SPI_execute(sql, false, 0) < 0)
		elog(ERROR, "kcprobe: the statement failed");
	if (SPI_processed > 0 && SPI_tuptable != NULL)
	{
		char	   *v = SPI_getvalue(SPI_tuptable->vals[0], SPI_tuptable->tupdesc, 1);

		ereport(NOTICE, (errmsg("kcprobe: %s", v ? v : "(null)")));
	}
	SPI_finish();
	if (pushed)
		PopActiveSnapshot();
}

static void
probe_callback(SubXactEvent event, SubTransactionId mySubid,
			   SubTransactionId parentSubid, void *arg)
{
	if (event != SUBXACT_EVENT_PRE_COMMIT_SUB ||
		probe_sql == NULL || probe_sql[0] == '\0')
		return;
	probe_run(probe_sql);
}

static void
probe_xact_callback(XactEvent event, void *arg)
{
	if ((event != XACT_EVENT_PRE_COMMIT && event != XACT_EVENT_PRE_PREPARE) ||
		probe_commit_sql == NULL || probe_commit_sql[0] == '\0')
		return;
	probe_run(probe_commit_sql);
}

void
_PG_init(void)
{
	DefineCustomStringVariable("kcprobe.sql",
							   "The statement to run at the end of every subtransaction.",
							   NULL, &probe_sql, "",
							   PGC_USERSET, 0, NULL, NULL, NULL);
	DefineCustomStringVariable("kcprobe.commit_sql",
							   "The statement to run at the pre-commit and the pre-prepare of a transaction.",
							   NULL, &probe_commit_sql, "",
							   PGC_USERSET, 0, NULL, NULL, NULL);
	RegisterSubXactCallback(probe_callback, NULL);
	RegisterXactCallback(probe_xact_callback, NULL);
}
