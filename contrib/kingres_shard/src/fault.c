/*-------------------------------------------------------------------------
 *
 * fault.c
 *	  Fault injection for the tests of two-phase commit and recovery.
 *
 * The superuser-only setting kingres_shard.fault_injection names one point of
 * the commit protocol and what to do there:
 *
 *	  <point>:kill			kill this backend with SIGKILL (a crash: the
 *							postmaster restarts the node, nothing is cleaned)
 *	  <point>:error			raise an ERROR (a warning at the points after the
 *							local commit, where nothing may throw)
 *	  <point>:wait:<file>	sleep as long as <file> exists, so that a test can
 *							stop a node or kill a process at a known moment
 *
 * Points, in the order a committing transaction reaches them:
 *	  before_prepare, after_first_prepare, after_prepare, after_log (before the
 *	  local commit), after_commit (the local commit is durable, no COMMIT
 *	  PREPARED sent yet), between_commit_prepared (after the first one).
 *
 * The setting is empty by default.  It can only be set by a superuser, who can
 * already stop the server, so it is no new capability; it exists so that the
 * crash windows of the protocol can be tested deterministically.
 *
 * SPDX-License-Identifier: PostgreSQL
 * Portions Copyright (c) 2026, RooyeKhat Media Co
 *
 * IDENTIFICATION
 *	  contrib/kingres_shard/src/fault.c
 *
 *-------------------------------------------------------------------------
 */
#include "postgres.h"

#include <signal.h>
#include <unistd.h>

#include "miscadmin.h"
#include "utils/guc.h"
#include "utils/timestamp.h"

#include "node_conn.h"

static char *fault_spec = NULL;

void
kingres_shard_fault_init(void)
{
	DefineCustomStringVariable("kingres_shard.fault_injection",
							   "Developer setting: inject a failure at a point of the commit protocol.",
							   "Format: <point>:kill, <point>:error or <point>:wait:<file>.  Empty disables.",
							   &fault_spec,
							   "",
							   PGC_SUSET, GUC_NOT_IN_SAMPLE | GUC_SUPERUSER_ONLY,
							   NULL, NULL, NULL);
}

/* Points after the local commit, where no error may be raised. */
static bool
is_post_commit_point(const char *point)
{
	return strcmp(point, "after_commit") == 0 ||
		strcmp(point, "between_commit_prepared") == 0;
}

void
kingres_shard_fault_point(const char *point)
{
	char	   *spec;
	char	   *action;
	char	   *arg;
	size_t		plen;

	if (fault_spec == NULL || fault_spec[0] == '\0')
		return;
	plen = strlen(point);
	if (strncmp(fault_spec, point, plen) != 0 || fault_spec[plen] != ':')
		return;

	spec = pstrdup(fault_spec + plen + 1);
	action = spec;
	arg = strchr(spec, ':');
	if (arg != NULL)
		*arg++ = '\0';

	if (strcmp(action, "kill") == 0)
	{
		kill(MyProcPid, SIGKILL);
		for (;;)
			pg_usleep(1000000L);
	}
	else if (strcmp(action, "error") == 0)
	{
		if (is_post_commit_point(point))
			ereport(WARNING, (errmsg("injected failure at %s", point)));
		else
			ereport(ERROR,
					(errcode(ERRCODE_INTERNAL_ERROR),
					 errmsg("injected failure at %s", point)));
	}
	else if (strcmp(action, "wait") == 0 && arg != NULL)
	{
		TimestampTz start = GetCurrentTimestamp();

		while (access(arg, F_OK) == 0)
		{
			pg_usleep(20000L);
			if (!is_post_commit_point(point))
				CHECK_FOR_INTERRUPTS();
			if (TimestampDifferenceExceeds(start, GetCurrentTimestamp(), 120000))
				break;
		}
	}
	pfree(spec);
}
