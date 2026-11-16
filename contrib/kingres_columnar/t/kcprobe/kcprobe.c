/*-------------------------------------------------------------------------
 *
 * kcprobe.c
 *	  A module for tests of kingres_columnar only: it runs one SQL statement
 *	  from a callback of its own at the end of every subtransaction, in the
 *	  window between the two phases of the commit that the access method
 *	  uses to hand the rows of a subtransaction to its parent.
 *
 * Callbacks run last-registered first.  Loaded before kingres_columnar in
 * shared_preload_libraries, this module registers its callback first, so it
 * runs after the access method's callback for SUBXACT_EVENT_PRE_COMMIT_SUB:
 * the statement sees the buffers as phase one left them.
 *
 * The setting kcprobe.sql is the statement; its first result value is
 * reported as a NOTICE.  A statement that begins with "cursor:" is opened as
 * a cursor named kcc that outlives the callback, and one row is fetched.  Not installed, never part of a release.
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

static void
probe_callback(SubXactEvent event, SubTransactionId mySubid,
			   SubTransactionId parentSubid, void *arg)
{
	if (event != SUBXACT_EVENT_PRE_COMMIT_SUB ||
		probe_sql == NULL || probe_sql[0] == '\0')
		return;

	bool		pushed = false;

	/* RELEASE has no snapshot of its own, and SPI wants one */
	if (!ActiveSnapshotSet())
	{
		PushActiveSnapshot(GetTransactionSnapshot());
		pushed = true;
	}
	SPI_connect();
	if (strncmp(probe_sql, "cursor:", 7) == 0)
	{
		/* a cursor named kcc that outlives the callback, first row fetched */
		SPIPlanPtr	plan = SPI_prepare(probe_sql + 7, 0, NULL);
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
	if (SPI_execute(probe_sql, false, 0) < 0)
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

void
_PG_init(void)
{
	DefineCustomStringVariable("kcprobe.sql",
							   "The statement to run at the end of every subtransaction.",
							   NULL, &probe_sql, "",
							   PGC_USERSET, 0, NULL, NULL, NULL);
	RegisterSubXactCallback(probe_callback, NULL);
}
