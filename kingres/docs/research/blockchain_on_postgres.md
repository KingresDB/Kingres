<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Blockchain / ledger models on PostgreSQL and other relational databases (as of November 2026) - for KingresDB

Terminology used throughout (the term "blockchain model" is ambiguous):
- **Model 1 - Centralized tamper-evident ledger tables**: one operator, hash chain / Merkle tree over history, verification against externally stored digests (Azure SQL Ledger, Oracle blockchain tables, LedgerDB, immudb, QLDB).
- **Model 2 - Permissioned multi-node blockchain database**: several organizations each run a node; transactions are ordered by Raft/BFT and every node executes deterministically (IBM "Blockchain Meets Database", Postchain/Chromia, Kwil, BigchainDB, Veritas/FalconDB/BlockchainDB).
- **Model 3 - Anchoring digests to public chains / timestamp authorities**: OpenTimestamps, Ethereum contract (pg_credereum), RFC 3161, WORM storage.

Research caveat: ~25 tool calls were used. Items marked "(uncited, general knowledge)" appear only under Inferences and must not be treated as sourced facts. Several items are listed in Gaps because I found no source.

---

## 1. Existing examples of blockchain-style / ledger databases on PostgreSQL and comparable SQL engines (architecture, status, license, lessons)

### Takeaway
Only one serious academic system (IBM, VLDB 2019) forked PostgreSQL to build a Model-2 blockchain relational database, and I found no evidence it became a maintained open-source product. Living PostgreSQL-based Model-2 systems are application-chain products (Chromia/Postchain, Kwil). Model-1 on a SQL engine is mainstream in commercial products (SQL Server/Azure SQL, Oracle) and in immudb. pg_credereum is a 2018 prototype with little activity since. BigchainDB is discontinued. QLDB ended 2025-07-31.

### Cited Findings
**IBM Research - "Blockchain Meets Database" (VLDB 2019, Nathan, Govindarajan, Saraf, Sethi, Jayachandran) - Model 2**
- Describes a decentralized, replicated relational database with blockchain properties for a permissioned setting where mutually distrustful organizations each run a replica; two designs: commit order fixed before execution vs executing without knowing commit order while ordering runs in parallel; both rely on serializable snapshot isolation (SSI) — [DeepAI abstract](https://deepai.org/publication/blockchain-meets-database-design-and-implementation-of-a-blockchain-relational-database); [typeset.io PDF](https://typeset.io/pdf/blockchain-meets-database-design-and-implementation-of-a-2d8nzps9xf.pdf)
- Built by modifying PostgreSQL (it retains row versions), about 4,000 lines of C — [arXiv 1903.01919](https://arxiv.org/pdf/1903.01919)
- Order-then-execute (OE): consensus orders a block first, nodes execute the block's transactions concurrently under SSI but commit serially in block order. Execute-order-in-parallel (EOP): nodes execute as clients submit, ordering happens concurrently, clients name a snapshot block height, nodes re-execute missing transactions before commit — [ar5iv rendering of the paper](https://ar5iv.labs.arxiv.org/html/1903.01919)
- SSI modified: each row stores creator/deleter block numbers ("block-height SSI"); conflicts within a block resolved by block order; EOP forbids blind updates like `UPDATE t SET c = v` — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- Storage: block = sequence number, transactions, consensus metadata, previous block hash, own hash, orderer signature; blocks in an append-only file (`pgBlockstore`); transactions in catalog table `pgLedger`; certificates in `pgCerts`; client-signed transactions; malicious nodes detected via checkpoint-hash comparison — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- Determinism restrictions on stored-procedure "smart contracts": date/time, random, sequence, system-info functions restricted; `LIMIT`/`FETCH` require `ORDER BY` on primary key; `xmin`/`xmax` not allowed in WHERE; predicate reads must use an index else abort; `SELECT *` disallowed — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- Pluggable ordering service: Kafka+ZooKeeper (crash fault tolerant) and BFT-SMaRt (BFT); blocks cut by size or 1-second timeout — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- Reported performance (single DC, 3 orgs, ~196-byte txs): simple contract OE ~1,800 TPS, EOP ~2,700 TPS (~1.5x); serial Ethereum-style execution ~800 TPS; complex-join contract OE ~400 TPS; WAN adds ~100 ms latency with ~4% throughput loss; Kafka orderer throughput unaffected by orderer count, BFT-SMaRt falls from ~3,000 to ~650 TPS as orderers increase — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- Limitations stated by the authors: EOP shared-memory contention and higher commit time; checkpoint-hash flow not implemented in EOP prototype; pruning of historical rows (vacuum extension), privacy/channels left as future work — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- Status: research prototype; I did not find a public maintained release (see Gaps).

**Postchain / Chromia (ChromaWay) - Model 2, PostgreSQL-backed, alive**
- A Chromia node runs Postchain plus PostgreSQL; Postchain accepts transactions and queries, builds consensus within a cluster, interprets dapp code, builds blocks and talks to the relational DB; deploying a dapp auto-creates tables; Rell is compiled to an intermediate format and translated to SQL; each dapp runs in its own chain, scaling via clusters of independent sidechains; local dev requires PostgreSQL 16.3 — [Chromia node docs](https://docs.chromia.com/intro/about/architecture/node); [Chromia intro/architecture](https://docs.chromia.com/intro/architecture/node)
- ChromaWay's per-transaction update performance claims are marketing and unverified — [ChromaWay technology](https://www.chromaway.com/technology)
- Source conflict: some third-party listings call Chromia an L1 with an EVM L2; treat with caution — [dexscreener listing](https://dexscreener.com/bsc/0x635c26c473bb0686d403247477cc648a7c6edc2e)

**Kwil DB (Kwil Networks) - Model 2, PostgreSQL + CometBFT**
- Repo describes node software "built with PostgreSQL and CometBFT" for BFT networks on relational databases — [GitHub kwil-db](https://github.com/kwilteam/kwil-db)
- v0.7 (March 2024) moved the engine from SQLite to PostgreSQL — [Kwil v0.7 post](https://medium.com/kwildb/kwil-v0-7-open-source-postgresql-appchains-253470233827)
- Blog pitching "Fortifying Postgres with Byzantine Fault Tolerance" — [DEV Community](https://dev.to/kwilteam/fortifying-postgres-with-byzantine-fault-tolerance-19al)
- A fork (trufnetwork/kwil-db) references v0.10.0 and Go 1.23/1.24 — [fork mirror](https://gh.r-mj.com/trufnetwork/kwil-db). I did not confirm Kwil's 2026 release cadence or license from sources.
- CometBFT v0.39 (part of Cosmos "Ledger 2026.1") adds application-owned mempool ABCI changes and experimental libp2p; whether Kwil adopted it is unconfirmed — [Cosmos blog](https://cosmos.network/blog/cosmos-ledger-20261)

**BigchainDB - Model 2 (NoSQL), discontinued**
- Two layers: Tendermint-based consensus + MongoDB storage; DB-Engines states the open-source project is discontinued (no date given) — [DB-Engines](https://db-engines.com/en/system/BigchainDB%3BDrizzle%3BLeanXcale%3BTigris)

**Tableland - Model 2/3 hybrid (SQLite subset, not Postgres)**
- Decentralized DB on SQLite subset; tables are ERC721 tokens on-chain; validator nodes run protocol off-chain — [Filecoin docs](https://docs.filecoin.io/build-on-filecoin/advanced/decentralized-databases); [Tableland docs](https://docs-k9cjp4ph4-tableland.vercel.app/fundamentals/architecture/protocol-design). No shutdown found; current status unverified.

**pg_credereum (Postgres Professional) - Model 1 + Model 3 prototype**
- README: "not production-ready", prototype for Credereum project; one "merklix" tree for all managed tables; background worker packs transactions into blocks; block hashes optionally uploaded to an Ethereum smart contract; PG10 only; one database per instance; GPL-3.0; motivation is preventing the owner from showing different DB versions to different users — [GitHub](https://github.com/postgrespro/pg_credereum); [README raw](https://raw.githubusercontent.com/postgrespro/pg_credereum/master/README.md); [pgconf.ru 2018 slides](https://pgconf.ru/media//2018/02/19/pgconfru-2018-credereum.pdf)
- Activity: essentially dormant since 2018 (ecosyste.ms lists last update Sep 2024) — [ecosyste.ms](https://awesome.ecosyste.ms/projects/github.com%2Fpostgrespro%2Fpg_credereum). Treat as abandoned.

**immudb (Codenotary) - Model 1 with PG wire protocol, alive**
- Release v1.11.0 dated 2026-04-23 focuses on PostgreSQL compatibility (validated against Django, SQLAlchemy, JDBC, pgAdmin), has an AST-based SQL rewriter in beta behind a flag, `immudb_state()` verification helper, and a diff query over a transaction range — [immudb release notes](https://docs.immudb.io/master/releasenotes)
- Merkle design (third-party description): persistent main Merkle tree over transaction hashes, per-transaction tree of KV entries; proofs via main tree plus linear proof to current state — [Substack deep dive](https://arriqaaq.substack.com/p/diving-into-the-internals-of-an-immutable-database)
- Note: immudb is its own storage engine speaking (a subset of) the PG protocol, not PostgreSQL. License not verified in these notes.

**Dolt (DoltHub) - versioned SQL on prolly trees (MySQL-compatible, not blockchain)**
- Dolt 2.0 released; storage is a copy-on-write sorted Merkle tree (prolly tree) whose node boundaries depend on content; Apache 2.0; ~20k GitHub stars Feb 2026; DoltLite (SQLite fork with prolly trees) reached beta Aug 2026 — [InfoQ](https://infoq.com/news/2026/07/dolt-version-control); [DoltHub 20k stars](https://www.dolthub.com/blog/2026-02-25-20k-stars/); [DoltLite beta](https://dolthub.com/blog/2026-08-31-doltlite-beta); [BetterStack guide](https://betterstack.com/community/guides/databases/dolt-git-version/)
- DoltHub published a guide on migrating from QLDB to Dolt — [DoltHub blog](https://www.dolthub.com/blog/2024-08-22-migrating-from-qldb-to-dolt)
- Relevance: structural-sharing Merkle trees give verifiable, diffable history with a cheap root hash per commit; prolly-tree/columnar tension noted by DoltHub — [DoltHub 2025-09](https://www.dolthub.com/blog/2025-09-10-challenges-with-prolly-trees-and-columnar-storage)

**Academic Model-1/2 systems**
- Veritas (CIDR 2019, Microsoft): "verifiable database tables" integrated into parties' local DBs, tamper-evident collaboration; prototype ~1,500 lines of C#; a VLDB 2022 hybrid-DB study reports Veritas (Kafka) near 30,000 TPS on 4 peers — [CIDR page](https://www.vldb.org/cidrdb/2019/veritas-shared-verifiable-databases-and-tables-in-the-cloud.html); [Colyer summary](https://blog.acolyer.org/2019/01/30/veritas-shared-verifiable-databases-and-tables-in-the-cloud/); [VLDB 2022 hybriddb](https://www.comp.nus.edu.sg/~ooibc/hybriddb-vldb22.pdf)
- FalconDB (SIGMOD 2020): blockchain consensus + verification interfaces for collaborative DBs, Merkle tree (secondary source) — [dbscholar](https://rmarcus.info/dbscholar/papers/h820d92822ca0e2e6)
- BlockchainDB (VLDB 2019): DB layer over blockchains, sharded shared DB on a classic blockchain — [Colyer-style secondary / VLDB 2022 study](https://www.comp.nus.edu.sg/~ooibc/hybriddb-vldb22.pdf)
- ChainifyDB (CIDR 2021) "How to get rid of your Blockchain and use your DBMS instead" - only title found.
- GlassDB (VLDB 2023), verifiable ledger DB via transparency — listed in search results only: [VLDB](https://mail.vldb.org/pvldb/volumes/16/paper/GlassDB%3A%20An%20Efficient%20Verifiable%20Ledger%20Database%20System%20Through%20Transparency)

### Inferences
- The only maintained PostgreSQL-embedded Model-2 products are application-chain platforms (Chromia, Kwil) that run vanilla PostgreSQL beside a consensus engine and execute through their own transaction layer, not through arbitrary SQL. The IBM approach (modify PG internals, SSI by block height) is the only one that retains full PG transactional semantics, and it is a research prototype. KingresDB, as a PG fork, could be positioned similarly to the IBM design.
- pg_credereum's abandonment and immudb's pivot to PostgreSQL compatibility suggest the market prefers Model 1 with external digests over Model 2.
- Lesson common to the academic systems: the cost lies in determinism restrictions and ordering service scaling (BFT throughput falls as nodes increase), not in hashing.

### Gaps
- No current (2025-2026) public repo/release found for the IBM blockchain-relational-DB prototype (IBM Research "Hyperledger-like" fork); license unknown.
- Tableland, Kwil, Chromia license and 2026 activity not verified. pgledger / pg_ledger-style double-entry accounting extensions (these are financial-ledger, not tamper-evident) were not researched. ProvenDB, AergoSQL, ChainifyDB/FalconDB code status not found. No 2024-2026 "newcomer" PG-based blockchain DB identified beyond immudb PG-compat and Dolt/DoltLite; searches were limited.

---

## 2. Commercial ledger features (design references)

### Takeaway
SQL Server/Azure SQL Ledger is the most complete and best documented design: per-transaction Merkle tree, blocks of up to 100K transactions chained by hash, digests stored externally, verification procedure. Oracle blockchain tables chain rows with signatures and retention. QLDB (journal + Merkle proofs) ended 2025-07-31 and AWS pointed to Aurora PostgreSQL with pgAudit/DAS/S3, which gives audit but not cryptographic verifiability.

### Cited Findings
**SQL Server 2022+/Azure SQL Ledger**
- Rows modified by a transaction are SHA-256 hashed in a Merkle tree giving a root per transaction; transaction roots are Merkle-hashed into a block root; block hash = hash(block root, previous block hash); digest = hash of latest block — [MS Learn ledger overview](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)
- Two table types: updatable ledger tables (system-versioned with a history table plus ledger view) and append-only ledger tables (updates/deletes blocked at API level); "ledger database" allows only ledger tables and cannot be converted back — [MS Learn overview](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)
- Digests can be stored in Azure Blob immutable storage, Azure Confidential Ledger or on-prem WORM; verification recomputes hashes from current data against supplied digests and reports all inconsistencies. Microsoft explicitly says an admin with machine control can bypass checks and edit files; ledger cannot prevent this but guarantees detection — [MS Learn overview](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)
- Block closes at 100K transactions; `sys.database_ledger_blocks` holds Merkle root and previous block hash; `sys.sp_generate_database_ledger_digest` closes the open block; automatic digest storage in Azure SQL DB (container `sqldbledgerdigests`) and SQL Server 2022 (Azure Storage only) — [MS Learn blocks view](https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-database-ledger-blocks-transact-sql); [digest management](https://learn.microsoft.com/sql/relational-databases/security/ledger/ledger-digest-management); [database ledger](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-database-ledger)
- Microsoft positions ledger against blockchain for multi-party flows: verifiable central data "without the complexity and performance implications that network consensus introduces" — [MS Learn overview](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)
- Paper: Antonopoulos et al., SIGMOD 2021, DOI 10.1145/3448016.3457558; whitepaper on Microsoft Research — [dbscholar](https://rmarcus.info/dbscholar/papers/hf46f417a300fd71d); [MS Research page](https://azure.microsoft.com/en-us/resources/research/sql-ledger-cryptographically-verifiable-data-in-azure-sql-database). Performance numbers not retrieved.

**Oracle Blockchain Tables / Immutable Tables**
- Blockchain tables are insert-only, rows chained by cryptographic hashes; deletion only via `DBMS_BLOCKCHAIN_TABLE.DELETE_EXPIRED_ROWS` after the retention period (a retention lock exists); `verify_rows` and sample Java verifier (Verify_Rows.java) exist; `USER_BLOCKCHAIN_TABLES` view shows retention, lock and hash algorithm; V2 adds schema evolution, delegate signatures and countersignatures; 32 system chains per instance, random chain per new row — [Oracle DBMS_BLOCKCHAIN_TABLE 23](https://docs.oracle.com/en/database/oracle/oracle-database/23/arpls/dbms_blockchain_table.html); [Morgan's Library](https://morganslibrary.org/reference/pkgs/dbms_blockchain_table.html); [Oracle samples](https://github.com/oracle/blockchain-table-samples); [Oracle 26 docs](https://docs.oracle.com/en/database/oracle/oracle-database/26/arpls/dbms_blockchain_table.html)
- Oracle marketing summary: "prevent unauthorized changes made using the database and detect unauthorized changes that bypass the database" — [Oracle admin guide](https://docs.oracle.com/en/database/oracle/oracle-database/21/admin/managing-tables.html)
- Immutable tables (retention-based protection without hash chain) are only referenced via a LiveLabs title — [LiveLabs](https://livelabs.oracle.com/ords/r/dbpm/livelabs/view-workshop?wid=4142)

**Amazon QLDB**
- End of support 2025-07-31; AWS recommended Aurora PostgreSQL; migration via export + DMS full load and ongoing replication — [AWS DB blog](https://aws.amazon.com/blogs/database/migrate-an-amazon-qldb-ledger-to-amazon-aurora-postgresql/); [InfoQ 2024-07](https://www.infoq.com/news/2024/07/aws-kill-qldb); [Classmethod](https://dev.classmethod.jp/articles/amazon-qldb-eos-announcement/)
- AWS highlights pgAudit, S3, Aurora Database Activity Streams as replacements for journal/history/streams; Aurora does not provide cryptographic verifiability — [Tutorials Dojo QLDB page](https://tutorialsdojo.com/amazon-quantum-ledger-database-qldb/) (secondary; AWS blog above is primary). Microsoft also published "Moving from QLDB to ledger in Azure SQL" — [mirror listing](https://app-pack.telkomuniversity.ac.id/?p=485228)
- QLDB journal/Merkle-proof internals: not retrieved (Gap).

**Alibaba/Ant LedgerDB (VLDB 2020)**
- Centralized ledger database (CLT as opposed to DLT) with tamper evidence and non-repudiation; components: batch accumulated Merkle tree (bAMT), clue index (clue skiplist), clue-counter MPT for clue (lineage) verification; follow-up ICDE 2022 "Ubiquitous Verification in Centralized Ledger Databases" proposes CM-Tree (reported 33x higher verification throughput) — [VLDB paper](https://www.vldb.org/pvldb/vol13/p3138-yang.pdf); [dbscholar](https://rmarcus.info/dbscholar/papers/h5920ac6c02822d38); [GitHub nusdbsystem](https://github.com/nusdbsystem/LedgerDatabase)

**Google Trillian / transparency logs**
- Tile-based logs: clients fetch tiles and build inclusion/consistency proofs themselves; Sunlight spec: full tiles 256 hashes wide (8,192 bytes), checkpoints (signed tree heads) served mutable, partial tiles tied to checkpoint sizes; Let's Encrypt introduced Sunlight in 2024 and expected tile support in Trillian — [transparency.dev tile logs](https://transparency.dev/articles/tile-based-logs/); [C2SP sunlight.md](https://www.github.com/C2SP/C2SP/blob/main/sunlight.md); [Let's Encrypt](https://letsencrypt.org/2024/03/14/introducing-sunlight)
- Proof format is RFC 6962 Merkle (RFC 9162 is successor, same tree construction - I found no source stating that explicitly in search; see Gaps).

### Inferences
- Design to copy from SQL Ledger: per-tx Merkle root over row hashes, block = (tx-root Merkle, prev block hash), digest = block hash, ledger view joining table+history, closing the block on digest generation.
- Design to copy from Oracle: retention-locked deletion, user signatures/countersignatures, multiple parallel chains to reduce insert contention (the 32 system chains hint at a serialization bottleneck of a single chain - inference).
- Transparency-log tiling is the right model for publishing a KingresDB ledger so third parties can verify inclusion without trusting the server.

### Gaps
- Exact Azure SQL Ledger canonical hashing format and overhead numbers (paper not read); Oracle row-hash input bytes; QLDB journal structure; Alibaba LedgerDB current availability.

---

## 3. Implementation techniques inside PostgreSQL

### Takeaway
Cited evidence here is thinner (mostly PG docs and blog patterns). Practical options: triggers (simplest, bypassable by superuser), commit-time capture via XactCallback/WAL, logical decoding for external ledger builder, or a custom rmgr/table AM for a fork. A fork has the advantage of putting ledger hashing in the commit path itself. Immutability by software cannot beat filesystem access; only external digest anchoring provides detection.

### Cited Findings
- Trigger-based hash chains: each row stores `prev_hash` and `row_hash` computed from row data plus prev_hash, using pgcrypto `digest()`; BEFORE INSERT trigger reads chain tail; modifying a row breaks all subsequent hashes; UPDATE/DELETE blocked by raising exceptions in triggers and by granting the app role only INSERT/SELECT — [AppMaster guide](https://appmaster.io/blog/tamper-evident-audit-trails-postgresql); [Postgres list thread md5 of previous row](https://www.postgrespro.com/list/thread-id/2352981)
- Even with append-only, privileged users can rewrite past rows; mitigation is periodic checkpoints (latest hash + row count) published to external sinks — [AppMaster guide](https://appmaster.io/blog/tamper-evident-audit-trails-postgresql); [Thalian ADR](https://thalian-docs-c86.pages.dev/audit-log-chain-of-custody/)
- Custom WAL resource managers: extension defines `RmgrData` and calls `RegisterCustomRmgr` in `_PG_init`; must be in `shared_preload_libraries`; IDs must be globally unique (use `RM_EXPERIMENTAL_ID` for dev, reserve on the wiki for release); must stay loaded as long as such WAL records exist; more flexible than Generic WAL, which does not support logical decoding — [PG docs custom-rmgr (15)](https://www.postgresql.org/docs/15/custom-rmgr.html); [PG docs (17)](https://www.postgresql.org/docs/17/custom-rmgr.html); [PG docs (19dev)](https://www.postgresql.org/docs/19/custom-rmgr.html)
- IBM design hooks into PG's tuple versions/SSI with block-height creator/deleter columns and a pgLedger catalog — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- pg_credereum uses a background worker to batch transactions into blocks and a single Merkle prefix tree over all managed tables — [GitHub](https://github.com/postgrespro/pg_credereum)
- SQL Server enforces append-only "at the API level" and keeps history in a system-versioned table — [MS Learn](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)

### Inferences (uncited, general knowledge - verify before relying)
- Hook comparison: (a) row triggers: easy, per-row hashing, but superuser can `ALTER TABLE ... DISABLE TRIGGER`/session_replication_role; chain-tail contention serializes writers; (b) XactCallback/RegisterXactCallback `XACT_EVENT_PRE_COMMIT`: lets a transaction compute its Merkle root before commit, but commit order across backends is not known until commit — block membership must be assigned by a post-commit step ordered by commit LSN or by a ledger sequence taken under a lock at pre-commit; (c) logical decoding output plugin: emits changes in commit order (by commit LSN) after commit, ideal for an out-of-band block builder/verifier, but asynchronous (blocks lag), and the ledger lives outside the transactional guarantee; (d) custom rmgr or table AM in the fork: strongest, can write ledger entries as WAL, replicate to standbys and have crash recovery rebuild chain state; (e) in a fork one can add hashing directly in heap_insert/update/delete paths and store tx hash in commit record.
- Block formation: order transactions by commit LSN (or a ledger sequence assigned at commit under a lightweight lock); block header = {height, prev_hash, merkle_root_of_tx_hashes, timestamp, optional state/app hash, signatures}. Per-row chains give easy local verification but contention; per-transaction Merkle trees (SQL Ledger style) allow row-level inclusion proofs with log(n) paths.
- Canonical serialization: hash a typed, length-prefixed encoding of column values in catalog order, with a version tag, independent of text output settings (DateStyle, extra_float_digits, bytea_output, locale); include schema/column identity; avoid hashing `::text` casts. Domain-separate leaf and node hashes (0x00/0x01 prefix as in RFC 6962).
- Immutability limits: superuser, `pg_write_server_files`, direct file edits, restoring older base backups, and replacing the binary cannot be prevented; defence = external digests + off-host verification + signed WAL archive, restricted superuser in managed deployments. Truncate/DROP can be blocked via event triggers or core hook in the fork; but not against someone replacing files.
- Client signatures: ed25519 signing in the client with public keys registered in a catalog; verifying inside PG needs an extension (pgcrypto does PGP/hash/HMAC, not ed25519 in standard builds - verify); in a fork, link libsodium/OpenSSL. Key management: server signing key in HSM/KMS; rotate with key-id in block header.
- Verification functions: `verify_ledger(from_block, to_block, digest[])` recomputing hashes; `ledger_inclusion_proof(table, pk)` returning Merkle path + block header; consistency proof between two digests (RFC 6962 style).

### Gaps
- No sourced details found on: XactCallback commit ordering vs commit LSN; logical decoding ordering guarantees (PG docs not fetched); ed25519 in pgcrypto; event trigger limits. These are in Inferences only and should be verified against PG docs.

---

## 4. External anchoring (Model 3): RFC 3161, OpenTimestamps, public chains, WORM

### Takeaway
Anchoring a periodically published digest (block hash / Merkle root) to an independent trust root is what actually defeats an insider rewriting history. Options: WORM storage, an RFC 3161 TSA, OpenTimestamps/Bitcoin, or an Ethereum contract.

### Cited Findings
- OpenTimestamps: calendar servers batch submitted hashes into a Merkle tree and commit the root to Bitcoin; proofs verifiable without the service; they give an upper time bound only (block timestamps ~2 h accuracy) — [IETF draft-fassbender-scitt-time-anchor](https://datatracker.ietf.org/doc/html/draft-fassbender-scitt-time-anchor-07)
- RFC 3161: trusted TSA signs a token binding hash to time; security depends on the TSA key, operation and cert chain; can be combined with OTS; RFC 3161 often required in EU regulated settings while OTS lacks formal legal recognition in some jurisdictions — [same IETF draft](https://datatracker.ietf.org/doc/html/draft-fassbender-scitt-time-anchor-07); [OriginStamp](https://originstamp.com/blog/what-is-blockchain-based-timestamping)
- Microsoft stores digests in immutable blob storage, Confidential Ledger, or WORM — [MS Learn](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)
- pg_credereum uploads block hashes to an Ethereum smart contract — [GitHub](https://github.com/postgrespro/pg_credereum)

### Inferences
- S3 Object Lock (compliance mode) is a straightforward WORM sink (AWS documentation not retrieved; uncited). Recommended: publish digests to at least two independent sinks (WORM bucket + OTS or TSA) and auditors' own copies (a "witness" model like transparency-log cosigning).
- Bitcoin/OTS anchoring costs essentially nothing per digest because of calendar aggregation; Ethereum contract writes cost gas per digest.

### Gaps
- No source found on S3 Object Lock semantics or on gas-cost numbers.

---

## 5. Multi-node consensus for a permissioned SQL blockchain

### Takeaway
Determinism is the main problem. Published systems restrict SQL (stored procedures only, no now()/random()/sequences, forced ORDER BY) and either pre-order then execute (OE) or speculatively execute then validate (EOP). Consensus choice: Raft/Kafka scales better than BFT (IBM measured BFT-SMaRt falling from ~3,000 to ~650 TPS as orderers increased). CometBFT/ABCI requires fully deterministic state transitions.

### Cited Findings
- IBM: OE vs EOP, restrictions, ordering service Kafka vs BFT-SMaRt, TPS numbers — see section 1 — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- Serial execution (Ethereum-style) ~800 TPS vs ~1,800-2,700 TPS for SSI-parallel on the same system — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- CometBFT: post-block state and FinalizeBlock results must depend only on prior state and the block; app hash returned from Commit; if validators holding >1/3 voting power disagree on app hash, consensus halts; mixed software versions can break determinism — [CometBFT ABCI requirements](https://github.com/cometbft/cometbft/blob/main/spec/abci/abci++_app_requirements.md); [Cosmos SDK lifecycle](https://docs.cosmos.network/sdk/next/learn/concepts/lifecycle); [CometBFT v1.0 concepts](https://docs.cometbft.com/v1.0/spec/abci/abci++_basic_concepts)
- Chromia/Postchain: per-dapp chains, consensus within clusters, Rell compiled to SQL (language-level restriction of nondeterminism) — [Chromia docs](https://docs.chromia.com/intro/about/architecture/node)
- Kwil: PostgreSQL + CometBFT — [GitHub](https://github.com/kwilteam/kwil-db)
- Veritas: ~30,000 TPS with Kafka ordering on 4 peers (hybrid DB study); trusted-ordering/verification design rather than full BFT — [VLDB 2022 hybriddb](https://www.comp.nus.edu.sg/~ooibc/hybriddb-vldb22.pdf)

### Inferences (uncited)
- Nondeterminism sources in PG to ban or virtualize: now()/clock_timestamp (substitute block timestamp), random(), gen_random_uuid, sequences/serial (derive IDs from block height + tx index), unordered scans (require ORDER BY on unique key), floating point/locale collations (pin ICU version, prefer numeric/integers, C collation), `pg_catalog` info functions, parallel query, hash-aggregate ordering, VACUUM/xmin-dependent visibility, extension versions. All nodes must run identical binary/PG major version and settings.
- Smart contracts = PL/pgSQL functions registered through an allow-list/"deploy" transaction; run in a restricted executor that rejects volatile functions (IBM's approach). Raft (e.g. etcd/raft) suffices when all orgs trust each other not to be Byzantine but not to crash; BFT needed when orgs may lie. App-hash = hash of the ledger head + state checkpoint.
- Integrating CometBFT: write a Go/C sidecar implementing ABCI that forwards ordered tx to PG (Kwil-style); embedding BFT inside the postmaster is much harder.

### Gaps
- No benchmarks found for Kwil or Chromia TPS; no Hyperledger Fabric TPS figures retrieved; HotStuff/PBFT specifics not researched.

---

## 6. Interaction with sharding

### Takeaway
I found no source covering ledger+sharding for PostgreSQL. The only sourced hint: Chromia runs independent chains per dapp/cluster for horizontal scale, and BlockchainDB is described as a sharded DB over a blockchain. The rest is design inference.

### Cited Findings
- Chromia scales through independent sidechains within clusters; each dapp has its own chain — [Chromia docs](https://docs.chromia.com/intro/about/architecture/node)
- BlockchainDB described as sharded shared database on a classic blockchain — [VLDB 2022 hybriddb](https://www.comp.nus.edu.sg/~ooibc/hybriddb-vldb22.pdf)
- Oracle uses multiple (32) system chains per instance for chain-level parallelism — [Oracle docs](https://docs.oracle.com/en/database/oracle/oracle-database/23/arpls/dbms_blockchain_table.html) (semantics per Oracle docs summary; reason is my inference)

### Inferences (uncited)
- Per-shard chains (each shard its own block chain and digest) avoid a global serialization point; a coordinator-level "super-chain" or Merkle tree over per-shard head hashes (digest aggregation, like a log of logs, or a Merkle "mountain" of shard digests) gives one global digest to anchor.
- Cross-shard (2PC) transactions: record the same global txid and the prepared hash in each participant shard's block; inclusion proof = shard-local Merkle path + shard block header + inclusion of that header in the aggregate digest. A global chain ordered by a single sequencer is simpler to verify but caps throughput at a single node's commit rate.
- Consensus-based Model 2 over shards: run one consensus group per shard (as Chromia does per cluster) and a beacon/system chain for cross-shard ordering.

### Gaps
- No literature found on cross-shard ledger proofs in PG-based systems (Citus, Shardman) - this is open design space.

---

## 7. Honest assessment: when is a blockchain database useful?

### Takeaway
Consensus is justified only when multiple mutually distrusting parties must write shared state without a trusted operator. For single-operator integrity/audit, ledger tables with externally anchored digests give nearly the same guarantee at far lower cost, as Microsoft itself argues and as skeptics echo.

### Cited Findings
- Wüst and Gervais, "Do you need a blockchain?": blockchain makes sense only when multiple mutually mistrusting entities want to change shared state; with a single writer a regular database is better and faster — [ePrint 2017/375](https://eprint.iacr.org/2017/375)
- Gerard's critique: append-only cryptographically tamper-proof ledgers are good, the "distributed" part is largely superfluous, permissioned chains are hard to justify over a central DB (cites WFP single-user private Ethereum) — [David Gerard](https://davidgerard.co.uk/blockchain/2018/02/10/do-you-need-a-blockchain-probably-less-than-wust-and-gervais-think-you-do/)
- Microsoft: many multi-party networks are "fundamentally centralized solutions"; ledger lets participants verify centrally hosted data without network consensus overhead; also useful as trusted off-chain store for a blockchain — [MS Learn](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)
- QLDB's discontinuation by AWS and its recommended replacement (plain Aurora PostgreSQL with audit tooling) show weak market pull for managed ledger-only databases — [InfoQ](https://www.infoq.com/news/2024/07/aws-kill-qldb); [AWS blog](https://aws.amazon.com/blogs/database/migrate-an-amazon-qldb-ledger-to-amazon-aurora-postgresql/) (the "weak pull" reading is my inference; AWS's stated reasons not retrieved)
- Reported costs of consensus: IBM BFT-SMaRt throughput drops with orderer count (3,000 to 650 TPS) — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919)
- Ledger cannot stop file-level tampering, only detect it — [MS Learn](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)

### Inferences
- A digest-only ledger detects but cannot prevent, and cannot detect a "split-view" attack (showing different histories to different auditors) unless digests are published/witnessed independently; pg_credereum's motivation and transparency-log consistency proofs address exactly this.
- Model 2 value exists mainly for consortia (supply chain, inter-bank) and "appchain" cases; it adds operational complexity (identical binaries, determinism, governance).

### Gaps
- No quantitative study found comparing ledger-table-plus-anchoring and BFT DBs on real-world adoption; AWS's stated reason for QLDB sunset not located.

---

## 8. Recommended phased design for KingresDB (synthesis; partly inference, anchored to sourced designs above)

### Takeaway
Build Model 1 first (cheap, high value, mirrors SQL Ledger), add signatures and external anchoring second, and treat multi-node consensus as an optional third phase modelled on the IBM OE/EOP design or a CometBFT/ABCI sidecar.

### Cited Findings (design references)
- Phase 1 reference: SQL Ledger architecture (updatable + append-only ledger tables, history table, per-tx Merkle, 100K-tx blocks, digest = block hash, verification SP) — [MS Learn](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview); [blocks view](https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-database-ledger-blocks-transact-sql)
- Phase 2 references: Oracle user/delegate signatures and retention locks — [Oracle](https://docs.oracle.com/en/database/oracle/oracle-database/23/arpls/dbms_blockchain_table.html); OTS and RFC 3161 — [IETF draft](https://datatracker.ietf.org/doc/html/draft-fassbender-scitt-time-anchor-07); tile-based verifiable log publication — [transparency.dev](https://transparency.dev/articles/tile-based-logs/)
- Phase 3 references: IBM OE/EOP on PG with SSI — [ar5iv](https://ar5iv.labs.arxiv.org/html/1903.01919); Kwil (PG + CometBFT) — [GitHub](https://github.com/kwilteam/kwil-db); CometBFT determinism requirements — [CometBFT spec](https://github.com/cometbft/cometbft/blob/main/spec/abci/abci++_app_requirements.md)

### Inferences (proposed plan, uncited)
- **Phase 1 - ledger tables + digests**: `CREATE TABLE ... WITH (ledger = append_only|updatable)`; hidden columns (ledger_txid, seq_in_tx, start/end), system-versioned history table; per-row canonical hash; per-tx Merkle root computed at pre-commit; block builder (background worker) groups committed transactions in commit order into blocks (`ledger_blocks`: height, prev_hash, tx_merkle_root, ts, hash); `pg_ledger_generate_digest()`; `pg_ledger_verify(digests)`; row inclusion proof function; block UPDATE/DELETE/TRUNCATE/DROP on ledger tables in core (not just triggers); protect ledger catalogs from DML even for superuser except through ledger code paths; dump/restore and logical replication handling; pg_upgrade compatibility of hash format (version tag).
- **Phase 2 - signed txs + anchoring**: client ed25519 signatures over canonical tx payload stored in tx entry (with registered public keys, key rotation, countersign by server); server block signature; scheduled digest export to S3 Object Lock + OpenTimestamps or RFC 3161; publish checkpoints and tiles for third-party verification; external verifier CLI that needs no trust in the DB.
- **Phase 3 - multi-node**: start with execute-in-block-order (OE) on a deterministic SQL subset: contract = allow-listed PL/pgSQL, ban volatile functions, block-time substitute for now(), deterministic IDs; ordering via Raft first (CFT), then CometBFT via ABCI sidecar for BFT; app hash = ledger head + periodic state checkpoint hash; sharding: per-shard chains plus aggregated digest. Evaluate EOP only after OE works.
- Decision rule for the roadmap: ship Phase 1-2 as the product; only start Phase 3 with a concrete consortium use case.

### Gaps
- No empirical data on the effort or overhead of implementing these in a PG fork; hashing overhead for SQL Ledger-like designs not retrieved.
