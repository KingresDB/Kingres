# contrib/kingres_sketch/t/006_fuzz.pl
#
# Corrupted values: truncated, extended, oversized, bit-flipped and random
# byte strings go through every way a sketch enters the server (the text input
# function, the cast from bytea, the binary input function through COPY, and
# sketch_kind(), which runs the validation of the deserial functions of the
# aggregates, kind 4 included).  Each one is either an error or a valid value;
# a valid value is then read by every function of its type and written back,
# and the bytes must be the same.  The server must not crash.  Run it on a
# server built with assertions (the default of kingres/dev/build.sh).
#
# The mutations come from a fixed seed, so a failure can be repeated.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('fuzz');
$node->init;
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');

# Valid seed values of every type and encoding.
$node->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
SELECT setseed(0.123);
CREATE TABLE seeds (kind text, label text, v bytea);
INSERT INTO seeds
SELECT 'hll', 'empty4', hll_empty(4)::bytea UNION ALL
SELECT 'hll', 'sparse4', hll_add_agg(i, 4)::bytea FROM generate_series(1, 2) i UNION ALL
SELECT 'hll', 'dense4', hll_add_agg(i, 4)::bytea FROM generate_series(1, 60) i UNION ALL
SELECT 'hll', 'sparse10', hll_add_agg(i, 10)::bytea FROM generate_series(1, 100) i UNION ALL
SELECT 'hll', 'dense10', hll_add_agg(i, 10)::bytea FROM generate_series(1, 5000) i UNION ALL
SELECT 'hll', 'dense14', hll_add_agg(i, 14)::bytea FROM generate_series(1, 50000) i UNION ALL
SELECT 'hll', 'dense18', hll_add_agg(i, 18)::bytea FROM generate_series(1, 100000) i UNION ALL
SELECT 'topn', 'empty', topn_empty(5)::bytea UNION ALL
SELECT 'topn', 'one', topn_add_agg(x, 5)::bytea FROM unnest(ARRAY['a']) x UNION ALL
SELECT 'topn', 'full', topn_add_agg(x::text, 8)::bytea FROM generate_series(1, 200) x UNION ALL
SELECT 'topn', 'long', topn_add_agg(repeat('é', 400 + x), 3)::bytea FROM generate_series(1, 5) x UNION ALL
SELECT 'topn', 'merged', topn_union(topn_add_agg(x::text, 4), topn_add_agg((x + 2)::text, 4))::bytea
  FROM generate_series(1, 30) x UNION ALL
SELECT 'tdigest', 'empty', tdigest_empty(10)::bytea UNION ALL
SELECT 'tdigest', 'one', tdigest_agg(1.5::float8, 20)::bytea UNION ALL
SELECT 'tdigest', 'few', tdigest_agg(x::float8, 20)::bytea FROM generate_series(1, 30) x UNION ALL
SELECT 'tdigest', 'many', tdigest_agg(random() * 1000, 100)::bytea FROM generate_series(1, 20000) x UNION ALL
SELECT 'tdigest', 'weighted', tdigest_weighted_agg(x::float8, x, 50)::bytea FROM generate_series(1, 100) x UNION ALL
SELECT 'tdigest', 'ties', tdigest_agg((x % 3)::float8, 10)::bytea FROM generate_series(1, 500) x UNION ALL
SELECT 'tdigest_state', 'percentile', t.v FROM (SELECT '\x'::bytea AS v) t WHERE false;
EOS

# The state of approx_percentile has no SQL form of its own: build it by
# appending the fraction to a digest and changing the kind byte to 4.
$node->safe_psql('postgres', <<'EOS');
INSERT INTO seeds
SELECT 'tdigest_state', label,
       set_byte(v, 2, 4) || '\x000000000000e03f'::bytea
  FROM seeds WHERE kind = 'tdigest';
EOS

# Mutations of every seed, in SQL, so they run inside the server next to the
# validators.  mode: 0 random bytes changed, 1 one bit flipped, 2 truncated,
# 3 extended with random bytes, 4 a 4 or 8 byte field of the header region set
# to an extreme value, 5 random bytes of random length, 6 two values spliced.
$node->safe_psql('postgres', <<'EOS');
SET search_path = public, kingres_sketch;
CREATE FUNCTION mutate(v bytea, other bytea) RETURNS bytea LANGUAGE plpgsql AS $$
DECLARE
  mode int := (random() * 6.999)::int;
  len int := length(v);
  p int;
  i int;
  ext bytea;
BEGIN
  IF mode = 0 THEN
    FOR i IN 1 .. 1 + (random() * 3)::int LOOP
      p := (random() * (len - 1))::int;
      v := set_byte(v, p, (random() * 255)::int);
    END LOOP;
  ELSIF mode = 1 THEN
    p := (random() * (len - 1))::int;
    v := set_byte(v, p, get_byte(v, p) # (1 << (random() * 7)::int));
  ELSIF mode = 2 THEN
    v := substr(v, 1, (random() * len)::int);
  ELSIF mode = 3 THEN
    ext := '\x'::bytea;
    FOR i IN 1 .. 1 + (random() * 20)::int LOOP
      ext := ext || set_byte('\x00'::bytea, 0, (random() * 255)::int);
    END LOOP;
    v := v || ext;
  ELSIF mode = 4 THEN
    p := (random() * 32)::int;
    IF p + 4 <= len THEN
      FOR i IN 0 .. 3 LOOP
        v := set_byte(v, p + i, (ARRAY[0, 255, 127, 128, 1])[1 + (random() * 4)::int]);
      END LOOP;
    END IF;
  ELSIF mode = 5 THEN
    v := '\x'::bytea;
    FOR i IN 1 .. (random() * 80)::int LOOP
      v := v || set_byte('\x00'::bytea, 0, (random() * 255)::int);
    END LOOP;
  ELSE
    p := (random() * len)::int;
    v := substr(v, 1, p) || substr(other, 1 + (random() * length(other))::int);
  END IF;
  RETURN v;
END $$;

-- read every function of the type over a value that was accepted, and write it back
CREATE FUNCTION exercise(kind text, v bytea) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF kind = 'hll' THEN
    PERFORM hll_cardinality(v::hll), hll_precision(v::hll), hll_is_empty(v::hll),
            hll_union(v::hll, v::hll), hll_add(v::hll, 12345), hll_fold(v::hll, 4), v::hll::text;
    IF (v::hll)::bytea <> v THEN RAISE EXCEPTION 'hll bytes changed'; END IF;
    IF hll_union(v::hll, v::hll)::bytea <> v THEN RAISE EXCEPTION 'hll union not idempotent'; END IF;
    IF hll_fold(v::hll, hll_precision(v::hll))::bytea <> v THEN RAISE EXCEPTION 'hll fold changed bytes'; END IF;
  ELSIF kind = 'topn' THEN
    PERFORM topn_total(v::topn), topn_capacity(v::topn), topn_add(v::topn, 'zz'::text),
            topn_union(v::topn, v::topn), v::topn::text;
    PERFORM item, frequency, max_error FROM topn_list(v::topn);
    IF (v::topn)::bytea <> v THEN RAISE EXCEPTION 'topn bytes changed'; END IF;
    IF topn_total(topn_union(v::topn, v::topn)) <> 2 * topn_total(v::topn) THEN RAISE EXCEPTION 'topn union total'; END IF;
  ELSE
    PERFORM tdigest_count(v::tdigest), tdigest_min(v::tdigest), tdigest_max(v::tdigest),
            tdigest_quantile(v::tdigest, 0.5), tdigest_quantile(v::tdigest, ARRAY[0, 0.3, 1]),
            tdigest_rank(v::tdigest, 1.0), tdigest_add(v::tdigest, 3.0, 2),
            tdigest_union(v::tdigest, v::tdigest), v::tdigest::text;
    IF (v::tdigest)::bytea <> v THEN RAISE EXCEPTION 'tdigest bytes changed'; END IF;
  END IF;
END $$;

CREATE TABLE fuzz_result (kind text, trials int, valid int, errors int, bad_code int, bad_state int);
EOS

my $trials = 20000;
foreach my $kind ('hll', 'topn', 'tdigest')
{
	$node->safe_psql('postgres', <<"EOS");
SET search_path = public, kingres_sketch;
SELECT setseed(0.5);
DO \$\$
DECLARE
  s record;
  n int := 0; valid int := 0; errors int := 0; bad_code int := 0; bad_state int := 0;
  m bytea;
  other bytea;
  kinds text[] := ARRAY['hll', 'topn', 'tdigest', 'tdigest_state'];
  k text;
  sqlstate_ text;
  ok boolean;
BEGIN
  FOR n IN 1 .. $trials LOOP
    SELECT * INTO s FROM seeds WHERE kind = '$kind' ORDER BY random() LIMIT 1;
    SELECT v INTO other FROM seeds WHERE kind = '$kind' ORDER BY random() LIMIT 1;
    m := mutate(s.v, other);
    -- the cast from bytea
    BEGIN
      PERFORM exercise('$kind', m);
      valid := valid + 1;
    EXCEPTION WHEN OTHERS THEN
      GET STACKED DIAGNOSTICS sqlstate_ = RETURNED_SQLSTATE;
      IF sqlstate_ NOT IN ('22023', '22003') THEN bad_code := bad_code + 1; END IF;
      errors := errors + 1;
    END;
    -- the text input function (softly: must give the same verdict)
    BEGIN
      ok := pg_input_is_valid('\\x' || encode(m, 'hex'), 'kingres_sketch.$kind');
      -- the verdict of the text input equals the verdict of the cast
      BEGIN
        PERFORM m::kingres_sketch.$kind;
        IF NOT ok THEN bad_state := bad_state + 1; END IF;
      EXCEPTION WHEN OTHERS THEN
        IF ok THEN bad_state := bad_state + 1; END IF;
      END;
    EXCEPTION WHEN OTHERS THEN
      bad_state := bad_state + 1;
    END;
    -- sketch_kind(): the validation of the aggregate states
    BEGIN
      k := sketch_kind(m);
      IF k <> '$kind' THEN bad_state := bad_state + 1; END IF;
    EXCEPTION WHEN OTHERS THEN
      GET STACKED DIAGNOSTICS sqlstate_ = RETURNED_SQLSTATE;
      IF sqlstate_ <> '22023' THEN bad_code := bad_code + 1; END IF;
    END;
  END LOOP;
  INSERT INTO fuzz_result VALUES ('$kind', $trials, valid, errors, bad_code, bad_state);
END \$\$;
EOS
}
# the state of approx_percentile (kind 4): only sketch_kind reads it from SQL,
# with the validation of the deserial function
$node->safe_psql('postgres', <<"EOS");
SET search_path = public, kingres_sketch;
SELECT setseed(0.6);
DO \$\$
DECLARE
  s record; other bytea; m bytea; k text; sqlstate_ text;
  valid int := 0; errors int := 0; bad_code int := 0;
BEGIN
  FOR n IN 1 .. $trials LOOP
    SELECT * INTO s FROM seeds WHERE kind = 'tdigest_state' ORDER BY random() LIMIT 1;
    SELECT v INTO other FROM seeds WHERE kind = 'tdigest_state' ORDER BY random() LIMIT 1;
    m := mutate(s.v, other);
    BEGIN
      k := sketch_kind(m);
      IF k NOT IN ('tdigest', 'tdigest_state') THEN bad_code := bad_code + 1; END IF;
      valid := valid + 1;
    EXCEPTION WHEN OTHERS THEN
      GET STACKED DIAGNOSTICS sqlstate_ = RETURNED_SQLSTATE;
      IF sqlstate_ <> '22023' THEN bad_code := bad_code + 1; END IF;
      errors := errors + 1;
    END;
  END LOOP;
  INSERT INTO fuzz_result VALUES ('tdigest_state', $trials, valid, errors, bad_code, 0);
END \$\$;
EOS

my $res = $node->safe_psql('postgres', 'SELECT kind, trials, valid, errors, bad_code, bad_state FROM fuzz_result ORDER BY kind');
diag("fuzz results (kind|trials|valid|errors|bad_code|bad_state):\n$res");
foreach my $row (split /\n/, $res)
{
	my ($kind, $t, $valid, $errors, $bad_code, $bad_state) = split /\|/, $row;
	is($valid + $errors, $t, "$kind: every mutation was an error or a valid value");
	cmp_ok($valid, '>', 0, "$kind: some mutations are still valid values ($valid)");
	cmp_ok($errors, '>', $t / 4, "$kind: many mutations are errors ($errors)");
	is($bad_code, 0, "$kind: every error has SQLSTATE 22023");
	is($bad_state, 0, "$kind: text input, cast and sketch_kind agree");
}

# sketch_kind names the kinds, state kind 4 included
is($node->safe_psql('postgres', "SELECT string_agg(DISTINCT kingres_sketch.sketch_kind(v), ',' ORDER BY kingres_sketch.sketch_kind(v)) FROM seeds"),
	'hll,tdigest,tdigest_state,topn', 'sketch_kind accepts the seeds of every kind');

# random byte strings of random length through the text input
$node->safe_psql('postgres', <<'EOS');
SELECT setseed(0.77);
CREATE TABLE random_input AS
  SELECT i, '\x' || encode(decode(string_agg(lpad(to_hex((random() * 255)::int), 2, '0'), ''), 'hex'), 'hex') AS txt
    FROM (SELECT i, generate_series(1, (random() * 200)::int) FROM generate_series(1, 3000) i) x GROUP BY i;
EOS
is($node->safe_psql('postgres', <<'EOS'), '0', 'random byte strings: no valid hll, topn or tdigest by accident (the magic byte)');
SELECT count(*) FROM random_input
 WHERE pg_input_is_valid(txt, 'kingres_sketch.hll') OR pg_input_is_valid(txt, 'kingres_sketch.topn')
    OR pg_input_is_valid(txt, 'kingres_sketch.tdigest');
EOS

# Binary input through COPY: one file with one corrupted value per COPY.
my $dir = PostgreSQL::Test::Utils::tempdir;
srand(20261115);
sub binary_copy_file
{
	my ($file, $bytes) = @_;
	open(my $fh, '>:raw', $file) or die "cannot write $file";
	print $fh "PGCOPY\n\377\r\n\0", pack('N', 0), pack('N', 0);
	print $fh pack('n', 1), pack('N', length($bytes)), $bytes, pack('n', 0xFFFF);
	close($fh);
}

sub perl_mutate
{
	my ($v) = @_;
	my $mode = int(rand(5));
	my $len = length($v);
	if ($mode == 0) { substr($v, int(rand($len)), 1) = chr(int(rand(256))) for 1 .. 1 + int(rand(3)); }
	elsif ($mode == 1) { my $p = int(rand($len)); substr($v, $p, 1) = chr(ord(substr($v, $p, 1)) ^ (1 << int(rand(8)))); }
	elsif ($mode == 2) { $v = substr($v, 0, int(rand($len))); }
	elsif ($mode == 3) { $v .= join('', map { chr(int(rand(256))) } 1 .. 1 + int(rand(20))); }
	else { my $p = int(rand(32)); substr($v, $p, 4) = pack('N', (0, 0xFFFFFFFF, 0x7FFFFFFF, 0x80000000)[int(rand(4))]) if $p + 4 <= $len; }
	return $v;
}

$node->safe_psql('postgres', <<'EOS');
CREATE TABLE bin_hll (v kingres_sketch.hll);
CREATE TABLE bin_topn (v kingres_sketch.topn);
CREATE TABLE bin_tdigest (v kingres_sketch.tdigest);
EOS
my $copies = 300;
foreach my $kind ('hll', 'topn', 'tdigest')
{
	my @seed = split /\n/, $node->safe_psql('postgres', "SELECT encode(v, 'hex') FROM seeds WHERE kind = '$kind'");
	my $script = '';
	for my $i (1 .. $copies)
	{
		my $v = pack('H*', $seed[ $i % @seed ]);
		my $m = ($i % 4 == 0) ? $v : perl_mutate($v);
		binary_copy_file("$dir/$kind.$i.bin", $m);
		$script .= "COPY bin_$kind FROM '$dir/$kind.$i.bin' (FORMAT binary);\n";
	}
	my ($ret, $out, $err) = $node->psql('postgres', $script, on_error_stop => 0);
	my $failed = () = $err =~ /ERROR:/g;
	my $stored = $node->safe_psql('postgres', "SELECT count(*) FROM bin_$kind");
	is($stored + $failed, $copies, "binary COPY of $kind: every file was stored or refused ($stored stored, $failed refused)");
	cmp_ok($failed, '>', 0, "binary COPY of $kind: corrupted values were refused");
	unlike($err, qr/server closed the connection|terminated|TRAP|could not receive/, "binary COPY of $kind: the server stayed up");
	# what was stored is readable
	$node->safe_psql('postgres', "SELECT kingres_sketch.sketch_kind(v::bytea) FROM bin_$kind");
}

# the log has no crash, assertion failure or signal
my $log = slurp_file($node->logfile);
unlike($log, qr/TRAP:|terminated by signal|server process .* was terminated|PANIC/, 'no crash or assertion failure in the server log');
$node->safe_psql('postgres', 'SELECT 1');

$node->stop;
done_testing();
