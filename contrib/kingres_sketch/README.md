<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_sketch

Approximate aggregates for KingresDB (a thin fork of PostgreSQL 18.7), version
1.0: an approximate distinct count (`hll`, HyperLogLog), the most frequent items
(`topn`, Space-Saving) and approximate percentiles (`tdigest`, a merging
t-digest). A sketch is a small value of a new type. It can be built with an
aggregate, stored in a column (for example in a rollup table), united with other
sketches, and read with a function. The aggregates support PostgreSQL's partial
aggregation (`COMBINEFUNC`, `SERIALFUNC`, `DESERIALFUNC`, `PARALLEL SAFE`), so a
parallel aggregate and a partitionwise aggregate over a partitioned table use
them without any other extension.

The design is section 9 of
[`kingres/docs/design/analytics-phase12-queries.md`](../../kingres/docs/design/analytics-phase12-queries.md).
Pushing the aggregates down to the shard groups of `kingres_shard` is not part
of this extension and not part of version 1.0.

The code is written from the published algorithms, not from another
implementation:

- HyperLogLog: P. Flajolet, E. Fusy, O. Gandouet, F. Meunier, "HyperLogLog: the
  analysis of a near-optimal cardinality estimation algorithm", AofA 2007;
  S. Heule, M. Nunkesser, A. Hall, "HyperLogLog in practice: algorithmic
  engineering of a state of the art cardinality estimation algorithm", EDBT 2013
  (the sparse representation); O. Ertl, "New cardinality estimation algorithms for
  HyperLogLog sketches", arXiv:1702.01284, 2017 (the estimator and the reduction
  of the precision).
- Space-Saving: A. Metwally, D. Agrawal, A. El Abbadi, "Efficient computation of
  frequent and top-k elements in data streams", ICDT 2005; P. Agarwal, G. Cormode,
  Z. Huang, J. Phillips, Z. Wei, K. Yi, "Mergeable summaries", PODS 2012.
- t-digest: T. Dunning, O. Ertl, "Computing extremely accurate quantiles using
  t-digests", arXiv:1902.04023, 2019.

The source of no other implementation of these algorithms was read or copied.
Licence: PostgreSQL License.

## Installing

The extension has a fixed schema, `kingres_sketch`, and is not relocatable. It
needs no `shared_preload_libraries` entry, no restart and no setting.

```sql
CREATE EXTENSION kingres_sketch;
SET search_path = public, kingres_sketch;   -- or qualify the names
```

The types and functions are not in `public`, so they never shadow or conflict
with another `hll`, `topn` or `tdigest` that is installed. Put `kingres_sketch`
after `public` in `search_path`, as above, so that tables created without a
schema name go to `public`.

## SQL interface

All names are in the schema `kingres_sketch`. Every function is `IMMUTABLE` and
`PARALLEL SAFE`. A function is strict (a NULL argument gives NULL) unless the
text says what it does with NULL.

### Types

| Type | What it holds | Text form |
|---|---|---|
| `hll` | A HyperLogLog sketch of a set of elements, precision 4 to 18. | `\x` and the hex digits of the bytes |
| `topn` | A Space-Saving summary of a multiset of strings, capacity 1 to 100000. | the same |
| `tdigest` | A t-digest of a multiset of `float8` values, compression 10 to 10000. | the same |

Each type has input, output, `send` and `receive` functions, and explicit casts
to and from `bytea` that validate the bytes. The binary form (`send`) is the same
bytes as the cast to `bytea`. A sketch has no comparison operator and no default
operator class: it cannot be a key, an index column or a `GROUP BY` column; use a
function that reads it.

### hll

| Function or aggregate | Result | Meaning |
|---|---|---|
| `hll_empty(precision int DEFAULT 14)` | `hll` | An empty sketch. |
| `hll_hash(anyelement)` | `bigint` | The 64-bit hash of an element (see "The hash"). |
| `hll_add(hll, anyelement)` | `hll` | The sketch with an element added. NULL element: unchanged. |
| `hll_add_hash(hll, bigint)` | `hll` | The sketch with a hash added. NULL: unchanged. |
| `hll_union(hll, hll)` | `hll` | The union. Error `22023` if the precisions differ. |
| `hll_fold(hll, precision int)` | `hll` | The sketch at a lower precision. |
| `hll_cardinality(hll)` | `bigint` | The estimate, rounded. |
| `hll_precision(hll)` | `int` | The precision. |
| `hll_is_empty(hll)` | `boolean` | True if no element was added. |
| `hll_add_agg(anyelement [, precision int])` | `hll` | The sketch of the non-NULL inputs. Precision 14 by default. |
| `hll_union_agg(hll)` | `hll` | The union of the sketches. All must have one precision. |
| `approx_count_distinct(anyelement [, precision int])` | `bigint` | The estimate of the number of distinct non-NULL inputs; `hll_cardinality(hll_add_agg(x))` in one step. |

`hll_add_agg(x, p)` takes the precision from the first row of the group and every
later row must give the same value (otherwise `22023`). With no rows, the
aggregates return an empty sketch of precision 14 (`approx_count_distinct`
returns 0). `hll_fold` gives exactly the sketch that would have been built at the
lower precision from the same elements; it is the only way to unite sketches of
different precisions.

### topn

| Function or aggregate | Result | Meaning |
|---|---|---|
| `topn_empty(capacity int DEFAULT 1000)` | `topn` | An empty summary. |
| `topn_add(topn, anyelement)` | `topn` | The summary with one item added. NULL item: unchanged. |
| `topn_union(topn, topn)` | `topn` | The merge. Error `22023` if the capacities differ. |
| `topn_list(topn, n int DEFAULT NULL)` | `TABLE (item text, frequency bigint, max_error bigint)` | The counters by frequency, descending, at most `n` of them (all if `n` is NULL). |
| `topn_total(topn)` | `bigint` | The number of items added, N. |
| `topn_capacity(topn)` | `int` | The capacity k. |
| `topn_add_agg(anyelement [, capacity int])` | `topn` | The summary of the non-NULL inputs. Capacity 1000 by default. |
| `topn_union_agg(topn)` | `topn` | The merge of the summaries. |

`frequency` is an upper bound of the true frequency and `frequency - max_error` a
lower bound. An item is its canonical text, so only types whose text does not
depend on a setting are accepted: `int2`, `int4`, `int8`, `numeric`, `text`,
`varchar`, `bpchar`, `name`, `uuid` and `bool`. Floating-point, date, time and
range types are refused with `0A000` ("cast the value to text with an explicit
format"). Two values that are equal under the equality of their type are one item:
`char(n)` values are compared without their trailing spaces, so `'a'::char(3)` and
`'a'::char(5)` are one item, and `numeric` values without their scale, so `1.5`
and `1.50` are one item (the item is the text `1.5`). Strings are compared by their
bytes, which is the equality of a deterministic collation only: a string of a
nondeterministic collation (where different bytes can be equal) is refused with
`0A000`, and `hll` is the sketch for such strings. An item longer than 1024 bytes
is an error (`54000`). The capacity of an aggregate is the same in every row of
a group; a NULL capacity is an error (see "Aggregates, partial aggregation and
NULL").

### tdigest

| Function or aggregate | Result | Meaning |
|---|---|---|
| `tdigest_empty(compression int DEFAULT 100)` | `tdigest` | An empty digest. |
| `tdigest_add(tdigest, x float8, weight bigint DEFAULT 1)` | `tdigest` | The digest with a value added. NULL `x` or weight: unchanged. |
| `tdigest_union(tdigest, tdigest)` | `tdigest` | The merge. Error `22023` if the compressions differ. |
| `tdigest_quantile(tdigest, q float8)` | `float8` | The value at fraction `q` (0 to 1); NULL for an empty digest. |
| `tdigest_quantile(tdigest, q float8[])` | `float8[]` | The same for several fractions; a NULL element gives NULL. |
| `tdigest_rank(tdigest, x float8)` | `float8` | The estimated fraction of the weight at or below `x`. |
| `tdigest_count(tdigest)` | `bigint` | The total weight (exact). |
| `tdigest_min(tdigest)`, `tdigest_max(tdigest)` | `float8` | The smallest and largest value (exact); NULL for an empty digest. |
| `tdigest_compression(tdigest)` | `int` | The compression. |
| `tdigest_agg(x float8 [, compression int])` | `tdigest` | The digest of the non-NULL inputs. Compression 100 by default. |
| `tdigest_weighted_agg(x float8, weight bigint, compression int)` | `tdigest` | The same with a weight per row (at least 1). |
| `tdigest_union_agg(tdigest)` | `tdigest` | The merge of the digests. |
| `approx_percentile(q float8, x float8 [, compression int])` | `float8` | The approximate value at fraction `q` of the non-NULL inputs `x`. |

`NaN` and infinite values are refused (`22023`). `-0` is stored as `0`, and a value that has the bit pattern of `-0` in its minimum, maximum, a centroid mean or the fraction of a state is refused on input, so that the bytes of a digest are a function of its content (one more rule of the validation). In
`approx_percentile` the fraction is read from the first row of a group and must
be the same constant in every row. It is a plain aggregate, not an ordered-set
aggregate: `percentile_cont(0.5) WITHIN GROUP (ORDER BY x)` is a different and
exact function.

### Other

| Function | Meaning |
|---|---|
| `self_test()` | One line of text that must be equal on every node that exchanges sketches (see "The hash"). |
| `sketch_kind(bytea)` | The kind (`hll`, `topn`, `tdigest`, or `tdigest_state` for the state of `approx_percentile`) of a byte string that is a valid sketch or aggregate state; error `22023` for any other bytes. It runs the validation of the input and deserial functions. |

## Aggregates, partial aggregation and NULL

Every aggregate has a state of type `internal`, a final function, and a combine
function, a serial function and a deserial function, and is `PARALLEL SAFE`
(`SSPACE` is 12288 for hll, 16384 for topn and 8192 for tdigest). PostgreSQL
therefore plans them with a Partial Aggregate under a Gather, and with a Partial
Aggregate per partition under an Append when `enable_partitionwise_aggregate` is
on. The serialized state of an aggregate is the same byte string as the value of
its type (the state of `approx_percentile` is a digest followed by the fraction,
with kind 4). `DISTINCT` and `ORDER BY` inside a call are not partial-capable, as
for the built-in aggregates.

NULL inputs are ignored. An aggregate over no rows returns an empty sketch
(`approx_percentile` returns NULL, `approx_count_distinct` returns 0). When an
aggregate has a precision, capacity or compression argument and no row is
aggregated, the empty sketch has the default parameter.

**A NULL precision, capacity or compression argument is an error (`22004`) in
every row**, also in a row whose element is NULL, so that the answer does not
depend on the order of the rows, on the plan or on the number of workers. Every
row of a group must give the same non-NULL value (otherwise `22023`; a partial
aggregate whose parts differ fails when they are combined). The fraction of
`approx_percentile` is different: a row with a NULL fraction or a NULL `x` is
ignored, in every order, and the first non-NULL fraction is the one of the group.

## Stored sketches and rollups

A sketch is an ordinary column value. A rollup table keeps one sketch per group
of rows, and a sketch built in a smaller time window can be united later with
`hll_union_agg`, `topn_union_agg` or `tdigest_union_agg`.

| | `hll` | `topn` | `tdigest` |
|---|---|---|---|
| Union of the same data twice | unchanged (idempotent) | frequencies add up | weights add up |
| Union is commutative and associative | yes, and the bytes are identical | the guarantees hold, the exact result depends on the order | the guarantees hold, the exact result depends on the order |
| Same rows in two parts, united | the sketch of the union of the rows | the guarantees of N = N1 + N2 | the guarantees of N = N1 + N2 |

A rollup job must therefore either rebuild a whole window and replace the stored
sketch, or add each source row to the rollup exactly once. Uniting a window with
itself leaves an `hll` as it was and counts every item and value twice in a
`topn` and a `tdigest`.

## hll

A sketch of precision `p` has `m = 2^p` registers. The 64-bit hash of an element
picks a register with its top `p` bits, and the register keeps the largest rank
seen (1 plus the number of leading zero bits of the other `64 - p` bits). The
estimate is Ertl's improved estimator, which needs no bias table and no switch
between linear counting and the raw estimate. A small sketch is stored as a
sorted list of its non-zero registers (the sparse form of Heule et al.), a larger
one as `m` registers of 6 bits.

| precision | registers | largest value, bytes | relative standard error 1.04 / sqrt(m) |
|---|---|---|---|
| 4 | 16 | 16 | 26% |
| 10 | 1024 | 772 | 3.25% |
| 12 | 4096 | 3076 | 1.63% |
| 14 (default) | 16384 | 12292 | 0.81% |
| 16 | 65536 | 49156 | 0.41% |
| 18 | 262144 | 196612 | 0.20% |

A sketch is sparse while four bytes per non-zero register are fewer than the
dense size, so an empty sketch is 4 bytes and a sketch of a few hundred elements
at precision 14 is a few kilobytes. The in-memory state of an aggregate is a list
of at most `0.19 * m` entries, and then `m` bytes (16 KB at precision 14, 256 KB
at precision 18).

The union is the register-wise maximum: it is commutative, associative and
idempotent, so the union of the sketches of any partition of the rows, with any
number of duplicates between the parts, has the same bytes as the sketch built
from all the rows by one backend. This is what a parallel aggregate relies on,
and it is tested.

The measured error is in "Accuracy". Counting is exact for very small sets (up to
10 elements in the tests) and the error follows `1.04 / sqrt(m)`.

## topn

The summary keeps `k` counters (item, count, error). An item that has a counter
increments it. A new item takes a free counter, or, when all `k` are in use, the
counter with the smallest count `c`: the item is replaced, the error becomes `c`
and the count `c + 1`. After N insertions every counter satisfies
`count - error <= true frequency <= count`, every error is at most `N / k`,
every item with a true frequency above `N / k` has a counter, and the counts add
up to at most N.

Two summaries of the same capacity are merged as in Agarwal et al.: the counts
and errors of an item that is in both are added; an item missing from a summary
that is full receives that summary's smallest count as additional count and
additional error; an item missing from a summary with free counters receives
nothing; the `k` largest counts are kept. The same guarantees hold after the
merge, with N the sum of the two N. (The theorem of the paper is stated for the
Misra-Gries summary. The argument for Space-Saving is direct and is written in
the header comment of `src/topn.c`; the tests check the guarantees after merges
in random orders.) The merge is not associative or commutative in its exact
result: two merge orders can keep different low-count items; the guarantees hold
for both.

Memory: in an aggregate a counter costs about 60 bytes plus the item. A value is
20 bytes plus 18 bytes and the item for each counter: about 30 KB for a full
summary of 1000 counters of short items, at most about 104 MB for capacity
100000 with items of 1024 bytes.

The working memory of a call is more than the value, and is not limited by
`work_mem`: the detoasted argument, the loaded state, the second state of a union
and the serialized result exist together. Measured on the largest value (capacity
100000, items of 1024 bytes, 104 MB), the peak memory of a backend running
`topn_union` of two such values was 436 MB, 4.4 times the value, including the
about 40 MB of an idle backend. The decoded state of a counter holds its item in a
chunk of its exact length. With parallel workers the figure is per worker. A
capacity and an item length that make such values are allowed; lower the
capacity if the memory is a concern.`topn_list` returns at most 100000 rows.

## tdigest

The merging t-digest keeps a sorted list of centroids (mean, weight) and a buffer
of points not yet merged (`5 * compression` points). When the buffer is full, or a
value is read, the buffer is merged into the centroids in one pass that joins
neighbours while the scale function `k(q) = compression / (2 pi) * asin(2q - 1)`
grows by at most 1 across the joined centroid. Centroids near the minimum and the
maximum are therefore small, and the rank error is small there. The digest keeps
the exact minimum, maximum and total weight. A quantile is read from the
piecewise linear curve through (0, minimum), the centroids (at their middle
weight) and (total, maximum); `tdigest_rank` is its inverse.

The number of centroids is about `0.6 * compression` in the measurements below
and never above the compression; a value with up to `2 * compression + 2`
centroids is accepted on input. A digest of compression 100 is about 1 KB, of
compression 10000 about 100 KB at most; the aggregate state adds the buffer
(`5 * compression * 16` bytes, 8 KB at compression 100).

A merge is not exact and not order independent: the union of partial digests
differs, within the bound, from the digest built by one backend. The
decisions of the merging pass use the C library's `asin`, so two platforms can
differ in the last digits of a centroid for the same input.

## Format

A sketch is a byte string, the same in a column, in `send` and `receive`, in the
cast to `bytea` and in the serialized state of an aggregate. All integers are
little-endian and all floating-point numbers are IEEE 754 binary64 stored as
their bit pattern in little-endian order, whatever the machine. The bytes are a
function of the content: two sketches with the same registers (hll), counters
(topn) or centroids (tdigest) have identical bytes.

Header, 4 bytes: byte 0 the magic `0x4B`, byte 1 the format version (1), byte 2
the kind (1 hll, 2 topn, 3 tdigest, 4 state of `approx_percentile`), byte 3 a
parameter. A reader that meets another format version raises `unsupported sketch
format N` and never reads the bytes as version 1.

- **hll**: byte 3 is the precision. After the header: nothing (empty); or
  `3 * 2^p / 4` bytes of registers, 6 bits each, register `i` in bits `6i` to
  `6i + 5` of the bit string whose bit `b` is bit `b mod 8` of byte `b div 8`
  (dense); or `4n` bytes with `n` entries `(j << 6) | rank` of the non-zero
  registers in strictly increasing order of `j` (sparse). The encoding is
  sparse if and only if `4n` is smaller than the dense size, which makes it
  determined by the registers and lets the length tell the two apart.
- **topn**: byte 3 is 0. Then `k` (u32), N (i64), the number of counters `n`
  (u32), and `n` counters sorted by count descending and item bytes ascending:
  item length (u16), count (i64), error (i64), the item bytes.
- **tdigest**: byte 3 is 0. Then the compression (u32), the number of centroids
  `n` (u32), the total weight (i64), the minimum and maximum (f64; both 0 when
  empty), and `n` centroids sorted by mean: mean (f64), weight (i64). Kind 4
  has the fraction (f64) after the last centroid.

The text form is `\x` followed by the hex digits of these bytes (lower case on
output, either case on input). A change of the format is a new format version and
a new extension version, with a note in the upgrade script.

## The hash

A sketch must depend on the values it was built from and on nothing else: the same
values must give the same sketch in another database, on another node and after a
dump and restore. So the hash of an element is chosen by the kind of its type, and
never depends on an object identifier (OID):

| Type | The hash |
|---|---|
| scalar types (integers, `numeric`, floating point, `text`, `varchar`, `bpchar`, `name`, `bool`, `uuid`, date and time types, `inet`, `jsonb`, `bytea`, and others) | the extended hash function of the default hash operator class of the type (`hash_extended_proc` of the type cache, for example `hashint4extended`, `hashtextextended`, `hash_numeric_extended`, `uuid_hash_extended`), seed 0 |
| enum | the hash of the bytes of the label, not of its OID |
| array | the dimensions, the lower bounds and the hashes of the elements in storage order; a NULL element has its own marker |
| range, multirange | the empty flag, for each bound the flags (infinite, inclusive) and the hash of the value |
| composite type, record | the hashes of the fields in order (dropped columns skipped), a NULL field has its own marker |
| domain | the hash of the base type |
| `oid`, `regclass`, `regproc`, `regtype` and every other `reg*` type, `oidvector`, and any array, range or composite type that contains one | refused with `0A000`: their hash is a number that is not the same after a restore |

The hash of a scalar is then finished with a fixed bijection of 64 bits (the
finalizer of MurmurHash3 after adding a constant), so that both the high bits (the
register of an hll) and the low bits (its rank) are well mixed. The containers
use the same finalizer to combine the hashes of their parts. A type without a hash
operator class is refused (`type X has no hash function`), and a type that cannot
be hashed is refused when the function or aggregate is called with that type, also
for a NULL value, so that a query does not work or fail depending on the data.
Two values that are equal under the equality of the type have the same hash, so
`bpchar` values of different lengths, `numeric` values of different scale and the
two zeros of `float8` are one element.

This was changed before the first release: an earlier build hashed an enum by its
OID (and arrays, ranges and records of enums with it), and hashed arrays, ranges
and records by PostgreSQL's own functions. Sketches built by that build for those
types are not comparable with the sketches of this one; sketches of the scalar types
are unchanged, and the format version stays 1. No migration is needed because the
extension has not been released.

Why this one: a sketch is only meaningful if every node that builds or reads it
hashes the same element to the same value, on every platform and PostgreSQL
major version, and the hash of a value of any built-in type is already defined
by PostgreSQL's own hash functions. The same functions decide the partition of a
hash-partitioned table, so a change of them between majors would break
`pg_upgrade` and dump and restore of such tables, which is why they are stable in
practice. **PostgreSQL does not state this as a promise for every function and
every major version.** The extension therefore does not rely on it silently:
`kingres_sketch.self_test()` returns one line with the hash of a fixed value of
each scalar type, of an array and an estimate over a fixed set, and a node whose line differs (another
major version that changed a hash function, a different encoding or byte order)
must not exchange sketches with the others.

What follows from this choice:

- The hash of byte strings (`text`, `varchar`, `bpchar`, `name`, `uuid`,
  `numeric`) and of a `float8` other than zero is PostgreSQL's `hash_bytes`, which
  reads the data in words of the machine's byte order (`src/common/hashfn.c` has
  one code path per byte order), so a big-endian node would hash these types
  differently from a little-endian one. Integer and `bool` hashes are computed
  from the value and do not depend on the byte order. Every KingresDB node of the supported platforms is
  little-endian; `self_test()` would show a difference. This was verified by
  reading the code, not on a big-endian machine.
- Integers of different widths hash alike: `hll_hash(42::int2) = hll_hash(42::int8)`.
- Text is hashed as its bytes in the database encoding, so the same text in two
  databases of different encodings (with non-ASCII characters) is not the same
  element. Text with a nondeterministic collation is hashed by its sort key, which
  can depend on the collation library version.
- `bpchar` ignores trailing spaces, `numeric` ignores trailing zeros (`1.0` and
  `1.00` are one element for `hll` and one item for `topn`), and `-0` and `0` are one
  `float8`.
- The self test uses the C collation for its text value.

## Untrusted input and limits

A sketch can arrive from a client (text or binary input, a cast from `bytea`,
`COPY`) and from another node (the state of a partial aggregate). Every reader
validates before it uses a length, a count or an index: the size against the
largest possible value of the kind (before anything is decoded), the header, the
parameters, the length against the parameters, the order and range of the sparse
entries, the register values, the invariants of a top-n summary (counts, errors,
total, order, unique items, items valid in the database encoding with no zero
byte), the centroid count, weights, order, finiteness and the total of a digest.
Memory is bounded by the size of the value and by the limits below. An invalid
value is an error: `22P02` from the text input function, `22P03` from the binary
input function, `22023` from a cast, a function or a deserial function; never a
crash or a read past the value. `pg_input_is_valid` works on all three types.

| Limit | Value |
|---|---|
| hll precision | 4 to 18 |
| topn capacity | 1 to 100000 |
| topn item | 1024 bytes |
| tdigest compression | 10 to 10000 |
| tdigest weight | 1 to 2^63 - 1; the total must fit in a `bigint` |
| largest value | hll 196612 bytes, topn about 104 MB, tdigest about 320 KB |
| counts | `bigint`; an overflow is `22003` |

## Security notes

No function is `SECURITY DEFINER`. The C functions look nothing up by name (the
hash function, the output functions and the types are found by OID), so they
carry no `search_path` setting: a setting would make PostgreSQL save and restore
a GUC at every call, and an aggregate calls its state function once per row. The
regression test `sketch_security` checks that every function is in the schema,
in C, immutable, parallel safe, not `SECURITY DEFINER` and without a setting. The
extension is not trusted: only a superuser creates it.

## Accuracy

The numbers below are measured by the tests in `t/` (fixed seeds: the hash is
fixed, so the results are the same on every run) and the tests assert the bounds.

**hll**: for each cardinality n, 200 data sets of n distinct integers (20 sets for
n = 10^6), at precision 10, 12, 14 and 16, the error is at most
`max(4 * 1.04 / sqrt(m) * n, 2)` in every one of the data sets. The largest
relative error seen over all data sets of an n, in percent:

| n | p = 10 | p = 12 | p = 14 | p = 16 |
|---|---|---|---|---|
| 1000 | 7.5 | 3.3 | 1.3 | 0.80 |
| 10 000 | 8.1 | 4.0 | 1.8 | 0.74 |
| 100 000 | 8.6 | 4.4 | 2.3 | 0.83 |
| 1 000 000 | 5.5 | 2.9 | 1.5 | 0.61 |
| bound, 4 * 1.04 / sqrt(m) | 13.0 | 6.5 | 3.25 | 1.63 |

For n up to 100 the error is at most 3 elements (n = 100, p = 14), and sets of up
to 10 elements are counted exactly at precision 14 and 16 in every data set. The
mean error over the data sets of one n is between -0.4% and 1.2% and within two
standard errors of the mean in every row (no visible bias), and the standard
deviation measured over 30 data sets of 100 000 elements was 2.9%,
1.3%, 0.69% and 0.32% at precision 10, 12, 14 and 16, below `1.04 / sqrt(m)`.

**topn**: Zipf streams of 10^6 items over 10^5 distinct values (exponents 0.8, 1.0,
1.2), capacity 100 and 1000: every returned counter satisfies the bounds
`frequency - max_error <= true <= frequency` and `max_error <= N / k`, every item
above `N / k` has a counter, also after merging 2 to 16 partial summaries in random
orders; the top 10 equal the true top 10 for exponent 1.2 at capacity 1000.

| exponent | k | counters with error 0 | largest error | N / k |
|---|---|---|---|---|
| 0.8 | 100 | 0 | 9920 | 10000 |
| 0.8 | 1000 | about 20 | 911 | 1000 |
| 1.0 | 100 | 2 | 8785 | 10000 |
| 1.0 | 1000 | about 62 | 660 | 1000 |
| 1.2 | 100 | about 10 | 6168 | 10000 |
| 1.2 | 1000 | about 116 | 339 | 1000 |

(The flatter the distribution, the closer the largest error gets to the bound
`N / k`. A count of "about" varies by a few between runs, because a parallel
aggregate merges the partial summaries in the order the workers finish.)

**tdigest**: four distributions of 10^6 values, compression 50, 100 and 500. The
rank error of `tdigest_quantile(q)` is the distance from q to the interval of exact
fractions of the data around the returned value. Largest error over
q = 0.001, 0.01, 0.1, 0.5, 0.9, 0.99, 0.999 (tails: q at or below 0.01 and at or
above 0.99), for a digest built by one backend and for the union of 16 digests:

| distributions | compression | largest tail error | largest body error | centroids |
|---|---|---|---|---|
| uniform, normal, log-normal | 50 | 0.0037 | 0.0032 | 29 to 33 |
| uniform, normal, log-normal | 100 | 0.0009 | 0.0010 | 58 to 62 |
| uniform, normal, log-normal | 500 | 0.00007 | 0.00011 | 288 to 298 |
| ties (half the values repeat 100 values) | 50 | 0.0042 | 0.0059 | 28 to 32 |
| ties | 100 | 0.0040 | 0.0013 | 59 |
| ties | 500 | 0.00036 | 0.0010 | 294 to 318 |

The union of 16 digests is the larger error of each row. On the "ties" data the
tail error stays near 0.004 at compression 50 and 100: the data have atoms of
0.5% of the weight, so no returned value has a rank closer than about half an
atom to a q between two atoms. The tests assert the largest values above with a
margin of 2.

## Limits and known behaviour

- Sketches of different precision, capacity or compression cannot be united
  directly. Only an `hll` can be folded to a lower precision.
- The estimate of `hll` is not exact; the estimate of a union equals the estimate
  of the sketch of the union of the data. `hll_cardinality` and `hll_union_agg`
  read the whole value: for precision 18 a call costs about a quarter of a
  megabyte of work.
- `tdigest_add` and `topn_add` read and write the whole value: use the aggregates
  to build a sketch from many rows.
- Parallel aggregation and partitionwise aggregation give the same sketch for
  `hll` and sketches within the bounds for the others: the exact counters of a
  `topn` and the exact centroids of a `tdigest` depend on the order of rows and
  merges.
- A `GROUP BY` with many groups multiplies the state size: the planner uses
  `SSPACE`; choose a smaller precision, capacity or compression for such a query.
- The aggregates have no moving-aggregate form (`MSFUNC`), so a window with a
  sliding frame recomputes the sketch for every row.
- Names that the standard reserves, like `precision`, are quoted in the
  signatures: use `hll_empty("precision" => 12)` for a named argument.

## Building and testing

Out of tree, against an installation:

```
make USE_PGXS=1 PG_CONFIG=/path/to/bin/pg_config
make USE_PGXS=1 PG_CONFIG=/path/to/bin/pg_config install
```

With the developer scripts of the repository, which build against the
installation made by `kingres/dev/build.sh`:

```
kingres/dev/ext.sh kingres_sketch all
```

The tests: `sql/` and `expected/` (pg_regress: `sketch_setup`, `sketch_hll`,
`sketch_topn`, `sketch_tdigest`, `sketch_io`, `sketch_security`) and `t/` (TAP:
the accuracy of each sketch, parallel and partitionwise aggregation, dump and
restore and `COPY`, and a fuzz test of corrupted values that should be run on a
server built with assertions).
