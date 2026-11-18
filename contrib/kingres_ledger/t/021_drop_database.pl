# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/021_drop_database.pl
#
# L15-01 of the fourth review of the guard: the README said a role that is not
# a superuser cannot remove the ledger "in a database that a superuser owns".
# A member of the owner role with INHERIT (and no SET) has the privileges of the
# owner, and PostgreSQL lets a role with the privileges of the owner of a
# database drop it, from another database, where no hook of this library runs.
#
# The extension cannot prevent that.  What it can do is say the truth: which
# roles that are not superusers can drop the database (kingres.ledger_status,
# columns database_droppers and database_droppable_by_non_superuser, and the
# warning of ledger_enable), computed with the rule of PostgreSQL's dropdb:
#
#   * the owner of the database, a role that has its privileges (membership
#     with INHERIT, directly or through a chain), and
#   * a role that can SET ROLE (membership with SET, directly or through a
#     chain) to one of those or to a superuser, because the current user is what
#     dropdb checks.
#
# The cases: a superuser owner without members, a member with INHERIT, a member
# with neither INHERIT nor SET, a member with SET only, a chain of two
# memberships, a chain with a link without INHERIT, a chain of SET, a
# superuser member, a database owned by a role that is not a superuser; and the
# reproduction of the reviewer (the drop itself, from another database).
# Whether this belongs in ledger_verify: no, see the README, exception 6.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('dropdb');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

$node->safe_psql('postgres', q{
	CREATE ROLE sowner SUPERUSER NOLOGIN;
	CREATE ROLE nsowner NOLOGIN;
	CREATE ROLE m_inh LOGIN;
	CREATE ROLE m_none LOGIN;
	CREATE ROLE m_set LOGIN;
	CREATE ROLE m_chain_a LOGIN;
	CREATE ROLE m_chain_b LOGIN;
	CREATE ROLE m_broken LOGIN;
	CREATE ROLE m_set_a LOGIN;
	CREATE ROLE m_set_b LOGIN;
	CREATE ROLE m_super SUPERUSER LOGIN;
	CREATE ROLE n_inh LOGIN;
});

# A database owned by $owner with the extension and a ledger table in it.
sub make_db
{
	my ($db, $owner) = @_;
	$node->safe_psql('postgres', "CREATE DATABASE $db OWNER $owner TEMPLATE template0");
	$node->safe_psql($db, q{
		CREATE EXTENSION kingres_ledger;
		CREATE TABLE first_t (id int PRIMARY KEY);
	});
	return;
}

# What ledger_status says, read by column name as the control plane does.
sub status
{
	my ($db) = @_;
	return $node->safe_psql($db, q{
		SELECT database_owner || ' ' || database_owner_is_superuser || ' '
		       || database_droppable_by_non_superuser || ' '
		       || database_droppers::text
		  FROM kingres.ledger_status});
}

my $n = 0;

# The text on stderr of ledger_enable of a new table in the database.
sub enable_stderr
{
	my ($db) = @_;
	my $t = 'tab_' . (++$n);
	my ($ret, $out, $err) = $node->psql($db, "CREATE TABLE $t (id int); SELECT kingres.ledger_enable('$t') IS NULL");
	is($ret, 0, "ledger_enable works in $db");
	return $err;
}

#--------------------------------------------------------------------------
# A superuser owner, and who may be added over time
#--------------------------------------------------------------------------
make_db('d1', 'sowner');
is(status('d1'), 'sowner true false {}', 'a superuser owner without members: nobody that is not a superuser can drop it');
my $err = enable_stderr('d1');
unlike($err, qr/can be dropped/, 'and ledger_enable does not warn');

$node->safe_psql('postgres', q{GRANT sowner TO m_none WITH INHERIT FALSE, SET FALSE});
is(status('d1'), 'sowner true false {}', 'a member without INHERIT and without SET has nothing of the owner: not listed');
unlike(enable_stderr('d1'), qr/can be dropped/, 'no warning for it');

$node->safe_psql('postgres', q{GRANT sowner TO m_super});
is(status('d1'), 'sowner true false {}', 'a member that is a superuser is not "a role that is not a superuser"');

$node->safe_psql('postgres', q{GRANT sowner TO m_inh WITH INHERIT TRUE, SET FALSE});
is(status('d1'), 'sowner true true {m_inh}',
	'a member with INHERIT and no SET: listed, with no change of the database (the roles change over time)');
like(enable_stderr('d1'),
	qr/WARNING:  database "d1" can be dropped, and the ledger with it, by roles that are not superusers: m_inh\b/,
	'ledger_enable warns, and names the role');

$node->safe_psql('postgres', q{GRANT sowner TO m_set WITH INHERIT FALSE, SET TRUE});
is(status('d1'), 'sowner true true {m_inh,m_set}',
	'a member with SET and no INHERIT can SET ROLE to the owner: listed');

$node->safe_psql('postgres', q{
	GRANT sowner TO m_chain_a WITH INHERIT TRUE, SET FALSE;
	GRANT m_chain_a TO m_chain_b WITH INHERIT TRUE, SET FALSE;
	GRANT m_chain_a TO m_broken WITH INHERIT FALSE, SET FALSE;
});
is(status('d1'), 'sowner true true {m_chain_a,m_chain_b,m_inh,m_set}',
	'a chain of two memberships with INHERIT: both ends listed; a chain with a link without INHERIT and without SET: not');

$node->safe_psql('postgres', q{
	GRANT sowner TO m_set_a WITH INHERIT FALSE, SET TRUE;
	GRANT m_set_a TO m_set_b WITH INHERIT FALSE, SET TRUE;
});
is(status('d1'), 'sowner true true {m_chain_a,m_chain_b,m_inh,m_set,m_set_a,m_set_b}',
	'a chain of SET: the role at its end can SET ROLE to the owner through the first one: listed');

like(enable_stderr('d1'), qr/by roles that are not superusers: m_chain_a, m_chain_b, m_inh, m_set, m_set_a, m_set_b\n/,
	'the warning lists the same roles as the view');

#--------------------------------------------------------------------------
# What the roles can really do: the reproduction of the review
#--------------------------------------------------------------------------
for my $v ('v_inh', 'v_none', 'v_set', 'v_chain', 'v_broken', 'v_setchain')
{
	$node->safe_psql('postgres', "CREATE DATABASE $v OWNER sowner TEMPLATE template0");
	$node->safe_psql($v, q{CREATE EXTENSION kingres_ledger; CREATE TABLE z (i int primary key); SELECT kingres.ledger_enable('z'); INSERT INTO z VALUES (1)});
}
sub try_drop
{
	my ($role, $db, $prefix) = @_;
	my ($ret, $out, $err) = $node->psql('postgres', "$prefix DROP DATABASE $db",
		extra_params => [ '-U', $role ]);
	my $gone = $node->safe_psql('postgres', "SELECT count(*) = 0 FROM pg_database WHERE datname = '$db'");
	return ($gone eq 't' ? 'dropped' : 'kept') . ($ret == 0 ? '' : ' (' . ($err =~ /must be owner of database/ ? 'must be owner' : $err) . ')');
}
is(try_drop('m_inh', 'v_inh', ''), 'dropped',
	'the reproduction of L15-01: a member with INHERIT and no SET drops a database that a superuser owns, ledger included');
is(try_drop('m_none', 'v_none', ''), 'kept (must be owner)',
	'a member without INHERIT and without SET cannot');
is(try_drop('m_set', 'v_set', ''), 'kept (must be owner)',
	'a member with SET only cannot drop it as itself');
is(try_drop('m_set', 'v_set', 'SET ROLE sowner;'), 'dropped',
	'but can after SET ROLE to the owner, which is why the view lists it');
is(try_drop('m_chain_b', 'v_chain', ''), 'dropped', 'the end of a chain of INHERIT drops it');
is(try_drop('m_broken', 'v_broken', ''), 'kept (must be owner)', 'a chain with a broken link cannot');
is(try_drop('m_set_b', 'v_setchain', 'SET ROLE sowner;'), 'dropped', 'the end of a chain of SET reaches the owner by SET ROLE');

#--------------------------------------------------------------------------
# L15-10: a member of the owner role cannot withdraw what PUBLIC may use
#--------------------------------------------------------------------------
{
	my ($ret, $out, $err) = $node->psql('d1',
		q{REVOKE EXECUTE ON FUNCTION kingres.ledger_row_hash(record) FROM PUBLIC},
		extra_params => [ '-U', 'm_inh' ]);
	isnt($ret, 0, 'a member of the owner role cannot revoke the EXECUTE of PUBLIC on a function of the ledger');
	like($err, qr/cannot revoke the privileges of PUBLIC on a function of the ledger/, 'with that message');
	($ret, $out, $err) = $node->psql('d1',
		q{REVOKE SELECT ON kingres.ledger_blocks FROM PUBLIC},
		extra_params => [ '-U', 'm_inh' ]);
	isnt($ret, 0, 'nor the SELECT of PUBLIC on a table');
	like($err, qr/HINT:  The read access that PUBLIC has on the catalog tables is withdrawn by a superuser/,
		'and the hint says why (it used to say that ledger tables are append-only)');
	unlike($err, qr/append-only/, 'no word about switching the ledger off');
	is($node->safe_psql('d1', q{SELECT has_function_privilege('public', 'kingres.ledger_row_hash(record)', 'EXECUTE')}),
		't', 'PUBLIC still has it');
	# Another grantee is harmless, and a superuser may do both (and puts them back).
	($ret) = $node->psql('d1', q{REVOKE EXECUTE ON FUNCTION kingres.ledger_row_hash(record) FROM m_none},
		extra_params => [ '-U', 'm_inh' ]);
	is($ret, 0, 'a REVOKE from another role is not refused');
	$node->safe_psql('d1', q{REVOKE EXECUTE ON FUNCTION kingres.ledger_row_hash(record) FROM PUBLIC;
		GRANT EXECUTE ON FUNCTION kingres.ledger_row_hash(record) TO PUBLIC});
	pass('a superuser can withdraw and restore it');
}

#--------------------------------------------------------------------------
# Everything taken back: the view follows
#--------------------------------------------------------------------------
$node->safe_psql('postgres', q{
	REVOKE sowner FROM m_inh, m_set, m_chain_a, m_set_a;
});
is(status('d1'), 'sowner true false {}', 'when the memberships are revoked the database is out of reach of every role that is not a superuser');
unlike(enable_stderr('d1'), qr/can be dropped/, 'and ledger_enable stops warning');

#--------------------------------------------------------------------------
# A database owned by a role that is not a superuser
#--------------------------------------------------------------------------
make_db('d4', 'nsowner');
is(status('d4'), 'nsowner false true {nsowner}', 'a non-superuser owner can drop its database: listed itself');
like(enable_stderr('d4'), qr/by roles that are not superusers: nsowner\n/, 'ledger_enable warns');
$node->safe_psql('postgres', q{GRANT nsowner TO n_inh WITH INHERIT TRUE, SET FALSE});
is(status('d4'), 'nsowner false true {n_inh,nsowner}', 'and so does every role that has its privileges');

# The control plane reads the view by column name: the columns of 0.3 are still there.
is($node->safe_psql('d4', q{SELECT count(*) FROM kingres.ledger_status
	WHERE worker_state IS NOT NULL AND database_owner IS NOT NULL}), '1',
	'the columns that were there are unchanged');

#--------------------------------------------------------------------------
# L17-02: roles that cannot drop the database now and can make themselves able
# to without a superuser: ADMIN OPTION on a role that can, directly or through
# a membership, and CREATEROLE for the roles the role created.  Listed in
# database_potential_droppers, warned about in a sentence of its own, and
# proved by real drops: the listing named each of them before it dropped.
#--------------------------------------------------------------------------
$node->safe_psql('postgres', q{
	CREATE ROLE nsown5 NOLOGIN;
	CREATE ROLE m_adm5 LOGIN;
	CREATE ROLE plain5 LOGIN;
	CREATE ROLE crr LOGIN CREATEROLE;
	CREATE ROLE nsown7 NOLOGIN;
	CREATE ROLE m_adm7 LOGIN;
	CREATE ROLE m_far7 LOGIN;
});

sub status2
{
	my ($db) = @_;
	return $node->safe_psql($db, q{
		SELECT database_droppers::text || ' ' || database_potential_droppers::text
		  FROM kingres.ledger_status});
}

make_db('d5', 'nsown5');
is(status2('d5'), '{nsown5} {}', 'a database owned by a role that is not a superuser, nobody administers the owner: no potential dropper');
$node->safe_psql('postgres', q{GRANT nsown5 TO m_adm5 WITH ADMIN TRUE, INHERIT FALSE, SET FALSE});
is(status2('d5'), '{nsown5} {m_adm5}',
	'a role with ADMIN OPTION on the owner and neither INHERIT nor SET: not a dropper, listed as one that can make itself');
{
	my $e = enable_stderr('d5');
	like($e, qr/WARNING:  database "d5" can be dropped, and the ledger with it, by roles that are not superusers: nsown5\n/,
		'ledger_enable warns about the droppers (the first sentence, as it was)');
	like($e, qr/WARNING:  database "d5" can also be dropped, and the ledger with it, by roles that are not superusers after they grant themselves a role they administer \(ADMIN OPTION[^)]*\): m_adm5\n/,
		'and, in a sentence of its own, about the roles that can make themselves droppers');
}
is(try_drop('m_adm5', 'd5', ''), 'kept (must be owner)', 'before it grants itself, the ADMIN OPTION holder cannot drop the database');
is(try_drop('m_adm5', 'd5', 'GRANT nsown5 TO m_adm5 WITH INHERIT TRUE, SET TRUE;'), 'dropped',
	'it grants itself the membership and drops it: the listing named it before');

# A role that administers a role that administers the owner (a membership with
# INHERIT carries the ADMIN OPTION of the role it is in).
make_db('d7', 'nsown7');
$node->safe_psql('postgres', q{
	GRANT nsown7 TO m_adm7 WITH ADMIN TRUE, INHERIT FALSE, SET FALSE;
	GRANT m_adm7 TO m_far7 WITH ADMIN FALSE, INHERIT TRUE, SET TRUE;
});
is(status2('d7'), '{nsown7} {m_adm7,m_far7}',
	'a role that has only the membership of a role that has the ADMIN OPTION is listed as well');
is(try_drop('m_far7', 'd7', 'GRANT nsown7 TO m_far7 WITH INHERIT TRUE, SET TRUE;'), 'dropped',
	'and it can make itself a dropper (the grant is allowed and the drop works)');

# CREATEROLE: the role that created a role has the ADMIN OPTION on it.
$node->safe_psql('postgres', q{SET ROLE crr; CREATE ROLE kid6 LOGIN; RESET ROLE});
make_db('d6', 'sowner');
$node->safe_psql('postgres', q{GRANT sowner TO kid6 WITH INHERIT TRUE, SET FALSE});
is(status2('d6'), '{kid6} {crr}',
	'the creator of a role that can drop the database (CREATEROLE): listed as a potential dropper');
is(try_drop('crr', 'd6', ''), 'kept (must be owner)', 'the creator cannot drop the database as it is');
is(try_drop('crr', 'd6', 'GRANT kid6 TO crr WITH INHERIT TRUE, SET TRUE;'), 'dropped',
	'it grants itself the role it created and drops the database that a superuser owns');
unlike(status2('d1'), qr/plain5/, 'a role with no ADMIN OPTION on anything is listed nowhere');

#--------------------------------------------------------------------------
# L17-03: a database owned by pg_database_owner belongs, in the database a
# session is connected to, to the owner of that database: every role that owns
# a database of the cluster can drop it.  Listed as droppers, with a warning.
#--------------------------------------------------------------------------
$node->safe_psql('postgres', q{
	CREATE ROLE oo LOGIN;
	CREATE ROLE oo_member LOGIN;
	GRANT oo TO oo_member WITH INHERIT TRUE, SET FALSE;
});
$node->safe_psql('postgres', q{CREATE DATABASE ddo OWNER oo TEMPLATE template0});
$node->safe_psql('postgres', q{CREATE DATABASE dbx OWNER pg_database_owner TEMPLATE template0});
$node->safe_psql('dbx', q{CREATE EXTENSION kingres_ledger; CREATE TABLE first_t (id int PRIMARY KEY)});
{
	my $listed = $node->safe_psql('dbx', q{SELECT database_droppers::text FROM kingres.ledger_status});
	like($listed, qr/\boo\b/, 'L17-03: the owner of another database is listed as a dropper of a database owned by pg_database_owner');
	like($listed, qr/\boo_member\b/, 'and so is a role with its privileges');
	unlike($listed, qr/pg_database_owner/, 'the role nobody logs in as is not what is reported');
	unlike($listed, qr/plain5/, 'a role that owns no database is not');
	is($node->safe_psql('dbx', q{SELECT database_droppable_by_non_superuser FROM kingres.ledger_status}), 't', 'and the flag is set');
	my $e = enable_stderr('dbx');
	like($e, qr/WARNING:  database "dbx" can be dropped, and the ledger with it, by roles that are not superusers: .*\boo\b/, 'ledger_enable warns');
	like($e, qr/DETAIL:  The database is owned by pg_database_owner/, 'and says what that owner is');
}
for my $d ('dpo1', 'dpo2', 'dpo3')
{
	$node->safe_psql('postgres', "CREATE DATABASE $d OWNER pg_database_owner TEMPLATE template0");
}
{
	my $drop = sub {
		my ($role, $from, $db) = @_;
		my ($ret, $out, $err) = $node->psql($from, "DROP DATABASE $db", extra_params => [ '-U', $role ]);
		my $gone = $node->safe_psql('postgres', "SELECT count(*) = 0 FROM pg_database WHERE datname = '$db'");
		return $gone eq 't' ? 'dropped' : 'kept';
	};
	is($drop->('oo', 'ddo', 'dpo1'), 'dropped', 'the reproduction of L17-03: the owner of ddo, connected to ddo, drops a database owned by pg_database_owner');
	is($drop->('plain5', 'postgres', 'dpo2'), 'kept', 'a role that owns no database cannot');
	is($drop->('oo_member', 'ddo', 'dpo3'), 'dropped', 'a role that has the privileges of the owner of ddo can, connected to ddo');
}

done_testing();
