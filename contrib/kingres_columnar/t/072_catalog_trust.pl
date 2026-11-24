# contrib/kingres_columnar/t/072_catalog_trust.pl
#
# XCAT-02: the stripe catalog is trusted only when it is the one of the extension.
#
# The library is preloaded, so its code runs in every database, and the stripe catalog is found by
# name in the schema of the extension.  The owner of a database can make that schema before the
# extension is installed, and put relations of the names of the catalog in it.
#
#   A. a database without the extension: a schema "kingres" with look-alike relations and a function
#      that writes a marker; ordinary work, an attempt to make a columnar table: no crash, no marker
#   B. the extension installed in a schema that a role that is not a superuser owns: the catalog is
#      refused with an error, and nothing is written to it
#   C. a catalog table that is not a member of the extension is refused; one of another shape too
#   D. the server did not restart
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('xct');
sub as_tenant
{
	my ($db, $sql) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql($db, $sql, stdout => \$stdout, stderr => \$stderr, extra_params => [ '-U', 'tenant' ], timeout => 60);
	return ($rc, $stdout, $stderr);
}
sub as_super
{
	my ($db, $sql) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql($db, $sql, stdout => \$stdout, stderr => \$stderr, timeout => 60);
	return ($rc, $stdout, $stderr);
}
sub log_text { return PostgreSQL::Test::Utils::slurp_file($node->logfile); }

my $started = q1($node, q{SELECT pg_postmaster_start_time()::text});
q1($node, q{CREATE ROLE tenant LOGIN CREATEDB; CREATE DATABASE spoofdb OWNER tenant; CREATE DATABASE ownerdb OWNER tenant});
q1($node, q{CREATE TABLE c (id int, v text) USING columnar; INSERT INTO c SELECT g, 'v' || g FROM generate_series(1, 20) g});

my $lookalike = q{
	CREATE SCHEMA kingres;
	CREATE TABLE public.marker (what text);
	CREATE FUNCTION public.mark() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO public.marker VALUES ('ran'); RETURN NULL; END $$;
	CREATE TABLE kingres.columnar_stripe (id int PRIMARY KEY, what text);
	CREATE TRIGGER t AFTER INSERT ON kingres.columnar_stripe FOR EACH ROW EXECUTE FUNCTION public.mark();
};

# A
my ($rc, $out, $err) = as_tenant('spoofdb', $lookalike);
is($rc, 0, 'A: the owner of a database without the extension makes a schema with a look-alike of the catalog') or diag($err);
($rc, $out, $err) = as_tenant('spoofdb', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v text);
	INSERT INTO public.t SELECT g, 'v' || g FROM generate_series(1, 10) g;
	UPDATE public.t SET v = 'x' WHERE id < 3;
	DELETE FROM public.t WHERE id = 5;
	VACUUM public.t;
	SELECT count(*) FROM public.t;
});
is($rc, 0, 'A: ordinary statements work') or diag($err);
like($out, qr/^9$/m, 'A: ... with the answers of PostgreSQL');
($rc, $out, $err) = as_tenant('spoofdb', q{CREATE TABLE public.col (id int) USING columnar});
isnt($rc, 0, 'A: there is no columnar access method here');
unlike($err, qr/server closed the connection|terminated/i, 'A: ... and no crash') or diag($err);
($rc, $out, $err) = as_super('spoofdb', q{SELECT count(*) FROM public.marker});
like($out, qr/^0$/m, 'A: nothing wrote a marker');

# B: the extension installed, and its schema then given to a role that is not a superuser (an install into a schema that such a
# role made first is refused by the install script where that check exists; this is the state that is left to the C code)
($rc, $out, $err) = as_super('ownerdb', q{CREATE EXTENSION kingres_columnar; ALTER SCHEMA kingres OWNER TO tenant});
is($rc, 0, 'B: the superuser installs the extension and gives its schema to the tenant') or diag($err);
($rc, $out, $err) = as_super('ownerdb', q{CREATE TABLE c (id int) USING columnar; INSERT INTO c VALUES (1)});
isnt($rc, 0, 'B: a columnar table cannot be written: the schema is not the extension\'s');
like($err, qr/cannot be used: the schema of the extension is not owned by a superuser/, 'B: ... the error says why') or diag($err);
unlike($err, qr/server closed the connection|terminated/i, 'B: ... and no crash');

# C: a table that is not a member of the extension (new sessions: the catalog is resolved once per session)
($rc, $out, $err) = as_super('postgres', q{
	BEGIN;
	ALTER EXTENSION kingres_columnar DROP TABLE kingres.columnar_stripe;
	SELECT count(*) FROM c;
	ROLLBACK;
});
isnt($rc, 0, 'C: a catalog table that is not a member of the extension is refused');
like($err, qr/cannot be used: it is not a member of the extension/, 'C: ... the error says why') or diag($err);
($rc, $out, $err) = as_super('postgres', q{
	BEGIN;
	ALTER TABLE kingres.columnar_stripe RENAME TO columnar_stripe_real;
	CREATE TABLE kingres.columnar_stripe (id int);
	ALTER EXTENSION kingres_columnar ADD TABLE kingres.columnar_stripe;
	SELECT count(*) FROM c;
	ROLLBACK;
});
isnt($rc, 0, 'C: a member table of another shape is refused');
like($err, qr/the stripe catalog has 1 columns|is not a member/, 'C: ... the error says why') or diag($err);
unlike($err, qr/server closed the connection|terminated/i, 'C: ... and no crash');
($rc, $out, $err) = as_super('postgres', q{INSERT INTO c VALUES (100, 'x'); SELECT count(*) FROM c});
is($rc, 0, 'C: the real catalog works') or diag($err);
like($out, qr/^21$/m, 'C: ... and the rows are there');

# C2. the right number of columns and the wrong types is refused; and what a session has seen is looked at again after
# ALTER EXTENSION ... DROP in the same session (the catalog was read first, so its OIDs were cached)
($rc, $out, $err) = as_super('postgres', q{
	BEGIN;
	ALTER TABLE kingres.columnar_stripe RENAME TO columnar_stripe_real;
	CREATE TABLE kingres.columnar_stripe (c1 bigint, c2 bigint, c3 bigint, c4 bigint, c5 bigint, c6 bigint, c7 bigint, c8 bigint,
	                                      c9 bigint, c10 bigint, c11 bigint, c12 bytea, c13 bytea, c14 boolean, c15 boolean,
	                                      CONSTRAINT columnar_stripe_pkey2 PRIMARY KEY (c1, c2));
	ALTER EXTENSION kingres_columnar ADD TABLE kingres.columnar_stripe;
	SELECT count(*) FROM c;
	ROLLBACK;
});
isnt($rc, 0, 'C2: the right number of columns with other types is refused');
like($err, qr/does not have the type this build expects|is not a member/, 'C2: ... the error says why') or diag($err);
($rc, $out, $err) = as_super('postgres', q{
	SELECT count(*) FROM c;
	BEGIN;
	ALTER EXTENSION kingres_columnar DROP TABLE kingres.columnar_stripe;
	SELECT count(*) FROM c;
	ROLLBACK;
});
isnt($rc, 0, 'C2: the catalog read before ALTER EXTENSION ... DROP is looked at again in the same session');
like($err, qr/is not a member of the extension/, 'C2: ... and found not to be a member') or diag($err);

# L1: CREATE on the schema granted to PUBLIC or to a role that is not a superuser: the catalog cannot be used either
for my $grantee ('PUBLIC', 'tenant')
{
	($rc, $out, $err) = as_super('postgres', qq{
		BEGIN;
		GRANT CREATE ON SCHEMA kingres TO $grantee;
		SELECT count(*) FROM c;
		ROLLBACK;
	});
	isnt($rc, 0, "L1: CREATE on the schema granted to $grantee: the catalog is refused");
	like($err, qr/cannot be used: the schema of the extension is not owned by a superuser, or allows/, 'L1: ... the error says why') or diag($err);
}

# D
is(q1($node, q{SELECT pg_postmaster_start_time()::text}), $started, 'D: the postmaster did not restart');
unlike(log_text(), qr/terminated by signal|was terminated|TRAP:/, 'D: no process ended by a signal');

$node->stop;
done_testing();
