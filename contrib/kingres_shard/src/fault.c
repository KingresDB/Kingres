/*-------------------------------------------------------------------------
 *
 * fault.c
 *	  Fault injection for the tests of two-phase commit and recovery.
 *
 * The superuser-only setting kingres_shard.fault_injection names one point of
 * the commit protocol and what to do there:
 *
 *	  <point>:kill[:n]		kill this backend with SIGKILL (a crash: the
 *							postmaster restarts the node, nothing is cleaned)
 *							(n: after letting n hits of the point pass)
 *	  <point>:error			raise an ERROR (a warning at the points after the
 *							local commit, where nothing may throw)
 *	  <point>:wait:<file>	sleep as long as <file> exists, so that a test can
 *							stop a node or kill a process at a known moment;
 *							<file>.reached is created when the point is reached
 *
 * A point name can carry a scope, <point>@<scope>:<action>: the point then fires
 * only in a session whose scope is that one.  The switch of a bucket move sets
 * the scope "switch" (kingres_shard_fault_set_scope), so that "after_commit@switch"
 * stops the commit of the switch and not the commit of the earlier steps of the
 * same move, which are two-phase transactions too.
 *
 * Points of a bucket move (bucket_move.c and the move_* functions of the
 * install script) are named move_<what>; the setting of the session that runs
 * a move is copied to every control session it opens, so a point inside a
 * function that runs on another node fires there.
 *
 *	  server_major:value:<n>	not a point of the protocol: the major version of
 *							PostgreSQL that this session reports as its own to the
 *							checks of kingres.add_node() (kingres_shard_server_major),
 *							so that a test can refuse a node of "another major"
 *							with two nodes of one.
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

#include "fmgr.h"
#include "miscadmin.h"
#include "utils/builtins.h"
#include "utils/guc.h"
#include "utils/memutils.h"
#include "utils/timestamp.h"

#include "node_conn.h"

static char *fault_spec = NULL;
static char *fault_scope = NULL;

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

/*
 * The scope of this session for the points that name one; it stays until the
 * session ends or another is set.
 */
void
kingres_shard_fault_set_scope(const char *scope)
{
	if (fault_scope != NULL)
		pfree(fault_scope);
	fault_scope = scope ? MemoryContextStrdup(TopMemoryContext, scope) : NULL;
}

/* The current value of the setting, "" when unset (copied to control sessions). */
const char *
kingres_shard_fault_spec(void)
{
	return fault_spec != NULL ? fault_spec : "";
}

/*
 * The major version of PostgreSQL this server runs, as the checks that keep one
 * major in a cluster see it (see the setting server_major:value:<n> above).
 */
int
kingres_shard_server_major(void)
{
	const char *prefix = "server_major:value:";

	if (fault_spec != NULL && strncmp(fault_spec, prefix, strlen(prefix)) == 0)
		return atoi(fault_spec + strlen(prefix));
	return PG_MAJORVERSION_NUM;
}

/*
 * SQL-callable form, for the PL/pgSQL parts of a bucket move:
 * kingres.internal_fault_point(point text).
 */
PG_FUNCTION_INFO_V1(kingres_internal_fault_point);

Datum
kingres_internal_fault_point(PG_FUNCTION_ARGS)
{
	char	   *point = text_to_cstring(PG_GETARG_TEXT_PP(0));

	kingres_shard_fault_point(point);
	PG_RETURN_VOID();
}

void
kingres_shard_fault_point(const char *point)
{
	char	   *spec;
	char	   *action;
	char	   *arg;
	size_t		plen;
	const char *rest;

	if (fault_spec == NULL || fault_spec[0] == '\0')
		return;
	plen = strlen(point);
	if (strncmp(fault_spec, point, plen) != 0)
		return;
	if (fault_spec[plen] == ':')
		rest = fault_spec + plen + 1;
	else if (fault_spec[plen] == '@')
	{
		/* <point>@<scope>:<action>: only in a session of that scope. */
		const char *colon = strchr(fault_spec + plen + 1, ':');
		size_t		slen;

		if (colon == NULL || fault_scope == NULL)
			return;
		slen = colon - (fault_spec + plen + 1);
		if (strlen(fault_scope) != slen ||
			strncmp(fault_scope, fault_spec + plen + 1, slen) != 0)
			return;
		rest = colon + 1;
	}
	else
		return;

	spec = pstrdup(rest);
	action = spec;
	arg = strchr(spec, ':');
	if (arg != NULL)
		*arg++ = '\0';

	/* kill and error take the number of hits to let pass first. */
	if ((strcmp(action, "kill") == 0 || strcmp(action, "error") == 0) && arg != NULL)
	{
		static char *counted_spec = NULL;
		static int	hits = 0;

		if (counted_spec == NULL || strcmp(counted_spec, fault_spec) != 0)
		{
			if (counted_spec != NULL)
				pfree(counted_spec);
			counted_spec = MemoryContextStrdup(TopMemoryContext, fault_spec);
			hits = 0;
		}
		if (hits++ < atoi(arg))
		{
			pfree(spec);
			return;
		}
	}

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
		char	   *reached = psprintf("%s.reached", arg);
		FILE	   *f;

		/* Tell the test that this point has been reached. */
		f = fopen(reached, "w");
		if (f != NULL)
			fclose(f);
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
