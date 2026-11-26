<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_sketch

Approximate aggregates for KingresDB (a thin fork of PostgreSQL; one source
builds and passes its tests on PostgreSQL 18.7 and 19.0), extension version 1.0
(the only version: `kingres_sketch--1.0.sql`): an approximate distinct count
(`hll`, HyperLogLog), the most frequent items (`topn`, Space-Saving) and
approximate percentiles (`tdigest`, a merging t-digest). A sketch is a small
value of a new type. It can be built with an aggregate, stored in a column (for
example in a rollup table), united with other sketches, and read with a
function. The aggregates support PostgreSQL's partial aggregation
(`COMBINEFUNC`, `SERIALFUNC`, `DESERIALFUNC`, `PARALLEL SAFE`), so a parallel
aggregate and a partitionwise aggregate over a partitioned table use them
without any other extension.

What it provides:

- three types, `hll`, `topn` and `tdigest`, with a byte format that is validated
  on every input (see "Format" and "Untrusted input and limits");
- functions that build, unite and read them, and aggregates that build and
  unite them, including `approx_count_distinct` and `approx_percentile`;
- a limit on the memory that the aggregate states of a backend may hold (the
  settings `kingres_sketch.state_memory_ceiling` and
  `kingres_sketch.state_memory_limit`);
- `self_test()`, a known-answer line that must be equal on every node that
  exchanges sketches.

Requirements: none beyond the installation. The extension needs no
`shared_preload_libraries` entry and no restart (preloading is advised, see
"Installing"), and only a superuser creates it. "Building and testing" at the
end says how to build it and run its tests on PostgreSQL 18 and 19.

The design is section 9 of
[`kingres/docs/design/analytics-phase12-queries.md`](../../kingres/docs/design/analytics-phase12-queries.md).
The extension itself knows nothing of shard groups. `kingres_shard` uses it:
when `kingres_shard.count_distinct_mode` is `approximate`, a `count(DISTINCT x)`
over a hash-distributed table is rewritten to
`kingres_sketch.approx_count_distinct`, and every shard group answers with a
small sketch (see [`limitations.md`](../../kingres/docs/guide/limitations.md) and
[`operator-guide.md`](../../kingres/docs/guide/operator-guide.md) in the guide).
The control plane installs the extension on every server and compares
`self_test()`.

Map of this document: "Installing"; "SQL interface" (the reference of the types,
functions and aggregates); "Aggregates, partial aggregation and NULL"; "Stored
sketches and rollups"; one section for each sketch ("hll", "topn", "tdigest");
"Memory of aggregate states and its limit" (the settings); "Format"; "The hash";
"Untrusted input and limits"; "Security notes"; "Accuracy" (measured errors);
"Limits and known behaviour"; "Building and testing".

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
Licence: GNU Affero General Public License, version 3 (AGPL-3.0-only).

## Installing

The extension has a fixed schema, `kingres_sketch`, and is not relocatable. It
needs no `shared_preload_libraries` entry and no restart. It has two settings,
`kingres_sketch.state_memory_ceiling` (the administrator's, only a superuser
sets it, default `1GB`) and `kingres_sketch.state_memory_limit` (a role's own,
default `-1`), with
defaults that leave ordinary queries alone (see "Memory of aggregate states and
its limit"). Preloading the library (`shared_preload_libraries`) is not needed,
but it makes the ceiling hold against every way a role can set it, see there.

```sql
CREATE EXTENSION kingres_sketch;
SET search_path = public, kingres_sketch;   -- or qualify the names
```

`CREATE EXTENSION` refuses a schema `kingres_sketch` that exists already when a
role that is not a superuser owns it, or when it grants `CREATE` to such a role
or to `PUBLIC` (SQLSTATE `42501`; see "Security notes"). The types and functions are not in `public`, so they never shadow or conflict
with another `hll`, `topn` or `tdigest` that is installed. Put `kingres_sketch`
after `public` in `search_path`, as above, so that tables created without a
schema name go to `public`.

## SQL interface

All names are in the schema `kingres_sketch`. Every function is `IMMUTABLE` and
`PARALLEL SAFE`, except `state_bytes()` and `state_limit()` (`VOLATILE`,
`PARALLEL RESTRICTED`). A function is strict (a NULL argument gives NULL) unless the
text says what it does with NULL.

### Types

| Type | What it holds | Text form |
|---|---|---|
| `hll` | A HyperLogLog sketch of a set of elements, precision 4 to 18. | `\x` and the hex digits of the bytes |
| `topn` | A Space-Saving summary of a multiset of strings, capacity 1 to 100000. | the same |
| `tdigest` | A t-digest of a multiset of `float8` values, compression 10 to 10000. | the same |

Each type has input, output, `send` and `receive` functions (`hll_in`, `hll_out`, `hll_recv`, `hll_send`, and the same
for `topn` and `tdigest`), and explicit casts to and from `bytea` that validate
the bytes (made by the functions `hll_to_bytea` and `bytea_to_hll`, and the same
for the other two types). The binary form (`send`) is the same
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
| `self_test()` | One line of text that must be equal on every node that exchanges sketches (see "The hash"; the control plane compares it, and an operator can run it by hand). |
| `state_bytes()` | The bytes held now by the aggregate states of the calling backend, the counter that the limit bounds (see "Memory of aggregate states and its limit"). `PARALLEL RESTRICTED`: it runs in the leader. |
| `state_limit()` | The limit in force in the calling backend, in bytes: the smaller of `kingres_sketch.state_memory_ceiling` and `kingres_sketch.state_memory_limit`. |
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
at precision 18). Working memory of a call at precision 18 (a value of 196612
bytes): `hll_union` of two values and `hll_union_agg` over six values need about
4 MB more than an idle backend (see "Untrusted input and limits").

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

Memory: in an aggregate a counter costs about 60 bytes plus the item (84 KB for a full
state of 1000 counters of short items, 1250 bytes per counter for items of 900
bytes; "Memory of aggregate states and its limit" has the figures of a query, which has one
state per group and per worker). A value is
20 bytes plus 18 bytes and the item for each counter: about 30 KB for a full
summary of 1000 counters of short items, at most about 104 MB for capacity
100000 with items of 1024 bytes.

The working memory of a call is more than the value, and is not limited by
`work_mem`: the detoasted argument, the loaded state, the second state of a union
and the serialized result exist together. Measured on the largest value (capacity
100000, items of 1024 bytes, 104200020 bytes), the peak memory of a backend
(`VmHWM`, which includes the 15.6 MB of an idle backend; 1 MB is 1048576 bytes):

| Call on the largest value | Peak of the backend | Times the value |
|---|---|---|
| `topn_union` of two values with the same items | 436 MB | 4.4 |
| `topn_union` of two values with different items | 491 MB | 4.9 |
| `topn_union_agg` over four values with different items | 513 MB | 5.2 |
| `topn_add_agg` of 100000 distinct items of 1024 bytes | 324 MB | 3.3 |
| `topn_list` of all counters | 329 MB | 3.3 |

Size a container for about 5.2 times the value plus the idle backend: 540 MB for
the largest value. The test `007_portable_sketches.pl` measures these calls and
fails when one needs more than 6.5 times the value. The decoded state of a
counter holds its item in a chunk of its exact length. With parallel workers the
figure is per worker. A capacity and an item length that make such values are
allowed; lower the capacity if the memory is a concern. `topn_list` returns at
most 100000 rows.

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
differ in the last digits of a centroid for the same input. `self_test()` shows
it: it builds two fixed digests from a formula, hashes the bytes of one and of
their union (the union is a second merging pass), and hashes the bits of
`asin` at 258 fixed arguments, which differ at once if the C library rounds
differently (a difference of one unit in the last place of `asin` changes the
hash; it changes a digest only when a merging decision falls on the boundary,
which is rare, so equal digests on two nodes do not prove equal `asin`, and the
`tdigest_scale` field is the sharper check).

Working memory of a call: the value, the loaded state and the result exist
together, so a union of two digests of compression 10000 (a value of about
100 KB, 320 KB at most) needs about 4 MB more than an idle backend, and
`tdigest_agg` of 300000 values at compression 10000 about 8 MB more (the buffer
of the aggregate included). See "Untrusted input and limits".

## Memory of aggregate states and its limit

The figures above are per call, or per state. **A query has one aggregate state
per group and per aggregate expression, and a parallel plan has more: every
worker builds a state of its own for the rows it reads, and the leader loads the
serialized state of every worker, keeps each until the group is finished, and
combines them into a state of its own.** `work_mem` does not limit any of it: a
hash aggregate checks its table against `work_mem` when a group is added, and a
state grows after its group was added. The `topn` aggregate with items of 900
bytes over 600000 rows and 300 groups held about 740 MB (a backend that was idle
at 19 MB went to 742 MB), and the same query with more groups or longer items
holds proportionally more. `hll` and `tdigest` grow the same way, only less per
state. What one state holds, measured with `state_bytes()` while the states are
alive (this includes the unused part of the blocks of the context):

| State | Held |
|---|---|
| a state with few values (`topn` of a few items, `hll` sparse, `tdigest` of a few values) | from under 100 bytes to about 1 KB; the counter moves in the steps of the blocks of the context, up to 8 MB |
| `topn`, capacity 1000, full, short items | 84 KB |
| `topn`, 20000 items of 900 bytes | 24 MB (1250 bytes per counter) |
| `hll`, precision 14, dense | 20 KB |
| `hll`, precision 18, dense | 260 KB |
| `tdigest`, compression 100 | 12 KB |
| `tdigest`, compression 10000, 300000 values | 885 KB |

So 100000 groups hold at least 300 MB, and 100000 groups of 100000 distinct
items hold 100000 times the figure of one. A worker is a backend with its own
account: with two workers the same query holds each worker's states in the
worker and the deserialized states, with the combined one, in the leader.

**The limit.** The limit of a backend is the smaller of two settings: the
**ceiling**, `kingres_sketch.state_memory_ceiling` (`PGC_SUSET`, default `1GB`,
from 1 kB to 2 TB; a plain number is in kilobytes), which only a superuser sets,
and the role's own **limit**, `kingres_sketch.state_memory_limit` (any role may
set it; the default `-1` means "the ceiling alone"). `state_limit()` returns the
limit in force, in bytes. The limit is the most that the aggregate states of one
backend may hold together. A query that would pass it fails with SQLSTATE `54000`
(`program_limit_exceeded`) and the message `aggregate states of kingres_sketch
hold N bytes in this backend, over the limit of M bytes`, with a hint, and the
backend's peak stays near the limit (the test `008_memory.pl`: with a limit of
64 MB the query of the review fails and a fresh backend goes from 19 MB to under
160 MB, where without a limit it went to 742 MB). The limit counts `hll`, `topn`
and `tdigest` states, `approx_count_distinct` and `approx_percentile` too, and
counts the states a worker builds, the states a leader deserializes from the
workers (before one is built, the length of the serialized state is checked
against what is left) and the states made by the combine and union aggregates.
It is checked when a state has grown, after the memory is taken, so a query can
pass the limit by one step of one state: the memory context of the states grows
in blocks of up to 8 MB, and the largest single step is a `topn` array
doubling, a few MB. The default of `1GB` is one that no ordinary query reaches
(the 300-group query above holds 740 MB); a server of a few GB that runs
parallel queries with many groups should set a lower ceiling, or lower
`max_parallel_workers_per_gather`. A parallel worker counts for itself, so a
parallel query can hold the limit once in the leader and once in each worker
(`1 + workers` times); the workers take the ceiling and the limit of their
leader, so no value that the leader's role could not set reaches them.

*All the states of one aggregate node share one memory context.* An earlier
version gave every state a context of its own so that its memory could be
counted; that cost more than a kilobyte per state and, because PostgreSQL walks
the child contexts of a hash aggregate on every new group, made a hash aggregate
of many groups unusably slow (50000 groups took 7.5 s, 100000 groups 77 s, a
million groups did not finish, against 1.0 s without any accounting; measured on
18.7). Now the states of the three types made under one aggregate context (the
whole node for a hash aggregate, one group for a sorted aggregate, which resets
its context after every group) live in one child context that carries the
account. One million small groups need the same memory and time as before the
limit existed; a sorted aggregate pays about 0.15 microseconds per group for
making the context.

*Who may set what.* The point of the ceiling is to protect the server, so a role
must not be able to raise it by any means, and any role may lower the limit for
its own queries. That rule is not made by a check of the value (a first version
judged the source of a value and a role walked around it with `ALTER ROLE
<itself> SET`: a check hook cannot know who stored a value) but by the context of
the setting, which PostgreSQL itself enforces at every place a value can enter:

| What a role does | Result |
|---|---|
| `SET`, `SET LOCAL`, `set_config` of the ceiling | `permission denied to set parameter` |
| `ALTER ROLE <itself or another role> SET` the ceiling, also `IN DATABASE`, also by a role with `CREATEROLE` | `permission denied to set parameter` |
| `ALTER DATABASE ... SET` the ceiling by a database owner who is not a superuser | `permission denied to set parameter` |
| the `SET` clause of a function the role owns (`CREATE FUNCTION`, `ALTER FUNCTION`) | `permission denied to set parameter` |
| the ceiling in the options of a connection (`PGOPTIONS`, `options=`) | the connection is refused when the library is preloaded; when it is not, see below |
| the role's own limit, by any of these ways | allowed, and it only lowers the limit: the limit in force is the smaller of the two |
| `RESET`, `RESET ALL` | go back to what the administrator gave |
| `SET ROLE` to an ordinary role | the session keeps the ceiling it had (a superuser's own `SET` stays); the ordinary role cannot raise it |
| a `SECURITY DEFINER` function of a superuser called by a role | runs with the settings of the caller's session; the role cannot raise the ceiling for it |
| a role that is a member of a superuser role and takes it (`SET ROLE`) | has the superuser's rights and may set the ceiling for that session: it is a superuser then, not an exception |
| a parallel worker | takes the values of its leader |

The administrator's ways work: `postgresql.conf`, `ALTER SYSTEM`, `ALTER
DATABASE ... SET` and `ALTER ROLE ... SET` by a superuser (a value for one role
may be higher than the server's, which is how an administrator gives a role
more). **`GRANT SET ON PARAMETER kingres_sketch.state_memory_ceiling TO r`**
gives `r` the right to set the ceiling, by `SET` and by `ALTER ROLE` and `ALTER
DATABASE`, and nobody else; it is the administrator's decision to hand the right
on. `REVOKE` does not remove a value the role has stored: the administrator
resets it (`ALTER ROLE r RESET ...`).

*A library that is not preloaded.* PostgreSQL treats a setting whose library is
not loaded yet as a placeholder: a role may `SET` it (the value is judged, and
dropped with a warning, when the library loads), but may not store it with
`ALTER ROLE`, `ALTER DATABASE` or a function's `SET` clause (only a superuser or
a holder of `GRANT SET ON PARAMETER` may); so before the library is loaded in a
session even the role's own limit cannot be stored (call a function of the
extension first, then `ALTER ROLE`). One path goes around that: a value in the
options of a connection becomes the reset value of the placeholder, and when the
library loads and refuses it, PostgreSQL has already dropped the administrator's
value, and the ceiling falls back to the built-in `1GB`, which can be higher
than the administrator's. The library sees this in `_PG_init` (a value was
waiting and none was accepted), warns, and the limit of that session is at most
1 MB; it can only lower the limit of the role that tried. To avoid the case
altogether, preload the library (`shared_preload_libraries`), where such a
connection is refused. Until the library is loaded in a session, `SHOW` of the
settings answers `unrecognized configuration parameter`.

*The counter does not leak.* The account lives in the memory context of the
states, and a reset callback of that context gives it back, so the counter is
correct whatever ends the states: the end of a group, the end of the query, an
error in the middle of a query, a subtransaction that aborts, a cursor that is
closed (a cursor that is left open holds its states and counts them until it is
closed). `state_bytes()` shows it, and the tests check that it is zero after a
completed query, a failed one, an aborted subtransaction, forty failures in a
row, a closed cursor, a sorted aggregate, `DISTINCT`, `GROUPING SETS`, a window
and a parallel query that fails in a worker or in the leader.

*What the limit does not count, and by how much a role can pass it.* Measured on
18.7 as the peak of a fresh backend (VmHWM, 15 MB idle) with values of the
largest `topn` size (100000 items of 1000 bytes, 101800020 bytes):

| Not counted | Peak above an idle backend |
|---|---|
| `topn_total` of one stored value | 211 MB (2.1 times the value) |
| `topn_add` of one item to a stored value | 318 MB |
| `topn_union` of two values | 321 MB; four values in one nested expression 753 MB: the working memory of a call is freed at the end of the row, so an expression holds the operands of all its calls at once |
| cast `bytea` to `topn` (a valid value of the largest size) | 313 MB |
| binary input (`COPY ... FORMAT binary`) of the largest value | 404 MB (4 times) |
| text input of the largest value (hex digits, twice the size; the figure includes writing the text first) | 1.1 GB |
| `hll_union`, `tdigest_union` of the largest values | 5 MB |
| the final function and the result of an aggregate (`topn` of 100000 items: counted 114 MB, peak 333 MB) | about 2 to 3 times the counted state, for the group that is being finished |

A hash aggregate that does not fit `work_mem` spills tuples, not states, and
processes the batches one after the other, resetting the aggregate context (and
so the account) for each; a query with 1000 groups of 200-byte items and a
`work_mem` of 64 kB peaked at 2 MB. What the limit does bound: **a value that
alone is larger than what is left of the limit is refused before it is
detoasted or unpacked** (`topn_union(a, b)` on 100 MB values under a limit of 16
MB fails with 54000 and the backend goes from 15 MB to 19 MB), by every function
that reads a sketch, by the input and receive functions and by the casts. So
under a limit smaller than the values the uncounted memory of one call is nil;
under a limit at least as large as the values it is up to about 4 times a value
per call (the table), times the number of large operands of one expression. The
values themselves are bounded by the sizes in "Untrusted input and limits"; an
administrator who needs a hard bound sets the ceiling to a fraction of the
memory available to a backend, and keeps the values that roles may store (for
example with a `CHECK` on `length(v::bytea)`) below it.

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

This was changed before version 1.0 was released: an earlier build hashed an enum by its
OID (and arrays, ranges and records of enums with it), and hashed arrays, ranges
and records by PostgreSQL's own functions. Sketches built by that build for those
types are not comparable with the sketches of this one; sketches of the scalar types
are unchanged, and the format version stays 1. No migration is needed because no
earlier version of the extension was released.

Why this one: a sketch is only meaningful if every node that builds or reads it
hashes the same element to the same value, on every platform and PostgreSQL
major version, and the hash of a value of any built-in type is already defined
by PostgreSQL's own hash functions. The same functions decide the partition of a
hash-partitioned table, so a change of them between majors would break
`pg_upgrade` and dump and restore of such tables, which is why they are stable in
practice. **PostgreSQL does not state this as a promise for every function and
every major version.** The extension therefore does not rely on it silently:
`kingres_sketch.self_test()` returns one line with the hash of a fixed value of
each scalar type, of an array, an estimate over a fixed set, and the hashes of
the bytes of a fixed `tdigest` and a fixed `topn` (each alone and united with a
second one) built from a formula. A node whose line differs (another major
version that changed a hash function, a different encoding or byte order, a C
library that rounds `asin` differently) must not exchange sketches with the
others. The control plane runs `self_test()` on every server when it installs or
verifies the extension (`kingres/control-plane/internal/ops/approx.go`) and
refuses a cluster whose servers differ (`409 sketch_hash_mismatch`). An operator
can also run `SELECT kingres_sketch.self_test()` on every node by hand. The fields: `int2` to `uuid` and `int4[]` the hash of a fixed value;
`hll` the estimate of a fixed set; `tdigest`, `tdigest_union`, `tdigest_read`
the bytes of a digest, of the union of two, and the bits of a quantile and a
rank read from it; `tdigest_scale` the bits of the scale function (`asin`) at
258 arguments; `topn`, `topn_union` the bytes of a summary and of the union of
two. The line is the same on PostgreSQL 18 and 19 (checked by the tests).

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
| memory held by the aggregate states of a backend | the smaller of `kingres_sketch.state_memory_ceiling` (superuser, default 1 GB) and `kingres_sketch.state_memory_limit` (a role's own); one state per group and per worker (see "Memory of aggregate states and its limit") |
| working memory of a call on the largest value (peak of the backend, an idle backend is 15.6 MB) | hll: about 4 MB over an idle backend (unions of precision 18, the aggregate over six values); topn: up to 513 MB, 5.2 times the value (`topn_union_agg` over values with different items; see the section "topn"); tdigest: about 4 MB over an idle backend for unions at compression 10000, 8 MB for `tdigest_agg` of 300000 values |
| counts | `bigint`; an overflow is `22003` |

## Security notes

No function is `SECURITY DEFINER`. The C functions look nothing up by name (the
hash function, the output functions and the types are found by OID), so they
carry no `search_path` setting: a setting would make PostgreSQL save and restore
a GUC at every call, and an aggregate calls its state function once per row. The
regression test `sketch_security` checks that every function is in the schema,
in C, immutable, parallel safe, not `SECURITY DEFINER` and without a setting
(`state_bytes()` and `state_limit()` are the two exceptions to "immutable" and
"parallel safe"). The extension is not trusted: only a superuser creates it.

The extension does not trust its schema. The install script refuses a schema
`kingres_sketch` that exists already when a role that is not a superuser owns it,
or when it grants `CREATE` to a role that is not a superuser or to `PUBLIC`
(`42501`, with a hint to drop the schema or revoke `CREATE`); otherwise that
role would own what a superuser reads and runs. The test `t/010_schema_spoof.pl`
checks the refusals and that a schema of a superuser is accepted. The control
plane makes the same check at run time (`extguard`) before it calls the
extension.

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

- The working memory of a call is several times the value for `topn` (up to 5.2
  times for `topn_union_agg` over values of the largest size, 513 MB for 104 MB
  values) and a few MB over an idle backend for `hll` and `tdigest`; see the
  tables in "topn" and "Untrusted input and limits".
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
- A `GROUP BY` with many groups multiplies the state size, and so do parallel
  workers; `work_mem` does not limit it, the limit of `kingres_sketch.state_memory_ceiling` does
  (see "Memory of aggregate states and its limit"); choose a smaller precision,
  capacity or compression for such a query.
- The aggregates have no moving-aggregate form (`MSFUNC`), so a window with a
  sliding frame recomputes the sketch for every row.
- Names that the standard reserves, like `precision`, are quoted in the
  signatures: use `hll_empty("precision" => 12)` for a named argument.

## Building and testing

Out of tree, against an installation of PostgreSQL 18 or 19 (one source builds on
both):

```
make USE_PGXS=1 PG_CONFIG=/path/to/bin/pg_config
make USE_PGXS=1 PG_CONFIG=/path/to/bin/pg_config install
make USE_PGXS=1 PG_CONFIG=/path/to/bin/pg_config installcheck   # pg_regress
```

With the developer scripts of the repository (see
[`kingres/docs/dev/testing.md`](../../kingres/docs/dev/testing.md)):

```
kingres/dev/ext.sh kingres_sketch all                 # one installation
KINGRES_PG18=<install> KINGRES_PG19=<install> kingres/dev/ext-majors.sh kingres_sketch   # clean build, both suites, on 18 and 19
```

The tests: `sql/` and `expected/` (pg_regress: `sketch_setup`, `sketch_hash`,
`sketch_hll`, `sketch_topn`, `sketch_tdigest`, `sketch_io`, `sketch_security`,
`sketch_memory`)
and `t/` (TAP: the accuracy of each sketch, parallel and partitionwise
aggregation, dump and restore and `COPY`, a fuzz test of corrupted values that
should be run on a server built with assertions, `008_memory.pl`: the limit
of the memory of aggregate states, the peak of a backend and parallel workers,
and `009_memory_ceiling.pl`: every way a role can try to raise the limit, each
followed by the next session of the role, on a server that preloads the library
and on one that does not, and `010_schema_spoof.pl`: the refusal of a schema that
another role owns or can create in).  The regression tests pass with and without the
library in `shared_preload_libraries`.
