// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package archive is the database archive of the data export (design data-export-import.md, sections "The archive", "The catalog document",
// "Hostile input" and "Versions and refusing an incompatible archive"; contract 0.40.0). It has three parts that are written against each other:
//
//   - Writer, a streaming zip writer for a non-seekable destination. It writes exactly one shape: deflate entries with a data descriptor (flag bit 3,
//     sizes and CRC zero in the local header, no extra field), the fixed entry names in a fixed order, and the manifest last.
//   - Validate, which accepts that shape and no other, reads each entry once and never writes a file or uses a name of the archive as a path.
//   - CheckCompatibility and BuildReport, which turn the manifest and the result of the validation into the reasons and the report of the contract.
//
// The package knows nothing of PostgreSQL, HTTP or the staging store. The SQL files are handed to a callback of the caller (the shape grammar plugs in there).
package archive

import (
	"fmt"
	"strings"
)

// The names of the entries. The data entries are DataName(1), DataName(2) and so on, contiguous from 1.
const (
	ManifestName = "manifest.json"
	CatalogName  = "catalog.json"
	PreDataName  = "schema/pre-data.sql"
	ViewsName    = "schema/views.sql"
	PostDataName = "schema/post-data.sql"
)

// Format is the value of Manifest.Format; CatalogVersion is the version of the catalog document this package writes and reads.
const (
	Format         = "kingres-export/1"
	FormatMajor    = 1
	CatalogVersion = 1
)

// The codes of the errors of this package; they are problem codes of the contract.
const (
	CodeInvalid      = "archive_invalid"
	CodeIncompatible = "archive_incompatible"
)

// The limits of the design ("Limits and timeouts").
const (
	MaxEntries       = 20000
	MaxManifestBytes = 4 << 20
	MaxCatalogBytes  = 16 << 20
	MaxSQLBytes      = 64 << 20
	MaxNameBytes     = 64
	MaxJSONDepth     = 32
	MaxArrayItems    = 100000
	MaxStringBytes   = 4096
	MaxComment       = 65535
	MaxBuckets       = 4096
)

// DataName is the name of the n-th data entry (n from 1).
func DataName(n int) string { return fmt.Sprintf("data/%05d.csv", n) }

// FixedNames are the entries that every archive has, in the order in which they are written; the data entries follow and the manifest is last.
var FixedNames = []string{CatalogName, PreDataName, ViewsName, PostDataName}

// IsSQL reports whether the name is one of the three SQL files.
func IsSQL(name string) bool { return name == PreDataName || name == ViewsName || name == PostDataName }

// IsData reports whether the name has the form data/NNNNN.csv and returns the number.
func IsData(name string) (int, bool) {
	if len(name) != len("data/00001.csv") || !strings.HasPrefix(name, "data/") || !strings.HasSuffix(name, ".csv") {
		return 0, false
	}
	n := 0
	for _, c := range name[5:10] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, n >= 1
}

// Reason is one thing that is wrong with an archive; it has the shape of a TableBlocker of the contract.
type Reason struct {
	Code    string
	Object  string
	Message string
}

// Error is the error of a validation: the code is CodeInvalid or CodeIncompatible and the reasons are fixed sentences with at most bounded names in them;
// nothing of the content of the archive is ever put in.
type Error struct {
	Code    string
	Reasons []Reason
}

func (e *Error) Error() string {
	if len(e.Reasons) == 0 {
		return e.Code
	}
	return e.Code + ": " + e.Reasons[0].Message
}

func invalid(code, object, format string, args ...any) *Error {
	return &Error{Code: CodeInvalid, Reasons: []Reason{{Code: code, Object: safe(object), Message: fmt.Sprintf(format, args...)}}}
}

// safe bounds and cleans a text that came from an archive before it is put in a message.
func safe(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= 48 {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// Manifest is manifest.json.
type Manifest struct {
	Format         string              `json:"format"`
	CreatedAt      string              `json:"createdAt"`
	Source         Source              `json:"source"`
	Extensions     []ManifestExtension `json:"extensions"`
	CatalogVersion int                 `json:"catalogVersion"`
	Requires       []string            `json:"requires,omitempty"`
	Fingerprint    string              `json:"fingerprint"`
	Entries        []ManifestEntry     `json:"entries"`
	Totals         Totals              `json:"totals"`
	Options        Options             `json:"options"`
	Warnings       []string            `json:"warnings"`
}

// Source is where the archive was made.
type Source struct {
	Database        string `json:"database"`
	KingresVersion  string `json:"kingresVersion"`
	ContractVersion string `json:"contractVersion"`
	PostgresMajor   int    `json:"postgresMajor"`
	GroupCount      int    `json:"groupCount"`
	Encoding        string `json:"encoding"`
	LcCollate       string `json:"lcCollate"`
	LcCtype         string `json:"lcCtype"`
	LocaleProvider  string `json:"localeProvider"`
}

// ManifestExtension is an extension of the source database with the schema it was installed in.
type ManifestExtension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Schema  string `json:"schema"`
}

// ManifestEntry describes one entry of the archive except the manifest itself. For a data entry Table, Columns, Rows and Consistency are set.
type ManifestEntry struct {
	Name          string   `json:"name"`
	Size          int64    `json:"size"`
	SHA256        string   `json:"sha256"`
	Table         string   `json:"table,omitempty"`
	Columns       []string `json:"columns,omitempty"`
	Rows          int64    `json:"rows,omitempty"`
	Consistency   string   `json:"consistency,omitempty"`
	Groups        int      `json:"groups,omitempty"`
	LedgerHistory bool     `json:"ledgerHistory,omitempty"`
}

// Totals are the sums the validator checks. Tables is the number of data entries that are not ledger histories, Rows the sum of their rows and
// UncompressedBytes the sum of the sizes of all entries except the manifest.
type Totals struct {
	Tables            int   `json:"tables"`
	Rows              int64 `json:"rows"`
	UncompressedBytes int64 `json:"uncompressedBytes"`
}

// Options are the choices of the request that made the archive. ExcludedTables are "schema.table" names whose rows are not in it.
type Options struct {
	DataMode       string   `json:"dataMode"`
	ExcludedTables []string `json:"excludedTables,omitempty"`
}

// Catalog is catalog.json: how the tables are distributed. It is structured data, never SQL.
type Catalog struct {
	CatalogVersion     int                 `json:"catalogVersion"`
	ColocationGroups   []ColocationGroup   `json:"colocationGroups"`
	Tables             []CatalogTable      `json:"tables"`
	DistributedSchemas []DistributedSchema `json:"distributedSchemas,omitempty"`
	TimepartPolicies   []TimepartPolicy    `json:"timepartPolicies"`
	Sequences          []CatalogSequence   `json:"sequences"`
}

// ColocationGroup is a colocation group by the reference tables use; the numeric id is not portable.
type ColocationGroup struct {
	Ref            string `json:"ref"`
	BucketCount    int    `json:"bucketCount"`
	DistColumnType string `json:"distColumnType"`
	Collation      string `json:"collation"`
}

// CatalogTable is one table. Columns are the columns of the table in order, which is the header of its data entry.
type CatalogTable struct {
	Schema             string            `json:"schema"`
	Name               string            `json:"name"`
	Kind               string            `json:"kind"`
	TreeRole           string            `json:"treeRole"`
	Parent             string            `json:"parent,omitempty"`
	Columns            []string          `json:"columns"`
	DistributionColumn string            `json:"distributionColumn,omitempty"`
	ColocationRef      string            `json:"colocationRef,omitempty"`
	UserPartitioning   *UserPartitioning `json:"userPartitioning,omitempty"`
	AccessMethod       string            `json:"accessMethod"`
	Columnar           map[string]string `json:"columnar,omitempty"`
	Ledger             *LedgerInfo       `json:"ledger,omitempty"`
	EstimatedRows      int64             `json:"estimatedRows"`
	EstimatedBytes     int64             `json:"estimatedBytes"`
}

// Qualified is "schema.name".
func (t CatalogTable) Qualified() string { return t.Schema + "." + t.Name }

// UserPartitioning is the partitioning that a user chose for a distributed table.
type UserPartitioning struct {
	Method     string          `json:"method"`
	Key        string          `json:"key"`
	Partitions []UserPartition `json:"partitions"`
}

// UserPartition is a leaf with its bound as PostgreSQL writes it.
type UserPartition struct {
	Name  string `json:"name"`
	Bound string `json:"bound"`
}

// LedgerInfo is what the archive says of a ledger table; the head is read without writing.
type LedgerInfo struct {
	Mode           string   `json:"mode"`
	Policy         string   `json:"policy"`
	HeadHeight     int64    `json:"headHeight"`
	HeadBlockHash  string   `json:"headBlockHash"`
	HistoryColumns []string `json:"historyColumns,omitempty"`
}

// DistributedSchema is a distributed schema (extension 0.8 and later).
type DistributedSchema struct {
	Schema     string `json:"schema"`
	Colocation string `json:"colocation"`
}

// TimepartPolicy is a time-partition policy; Enabled is as the source had it and is imported as false.
type TimepartPolicy struct {
	Schema          string            `json:"schema"`
	Table           string            `json:"table"`
	PartitionEvery  string            `json:"partitionEvery"`
	Timezone        string            `json:"timezone"`
	CreateAhead     string            `json:"createAhead"`
	Retention       string            `json:"retention"`
	RetentionAction string            `json:"retentionAction"`
	ColumnarAfter   string            `json:"columnarAfter"`
	ColumnarSort    string            `json:"columnarSort"`
	ColumnarOptions map[string]string `json:"columnarOptions,omitempty"`
	RunEvery        string            `json:"runEvery"`
	Enabled         bool              `json:"enabled"`
}

// CatalogSequence is a sequence owned by a column, with the state of the coordinator's sequence.
type CatalogSequence struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	OwnedBy   string `json:"ownedBy"`
	LastValue int64  `json:"lastValue"`
	IsCalled  bool   `json:"isCalled"`
}

// HasData reports whether a table has a data entry in an archive made with these options: not a partition leaf, not excluded, and data mode all.
func (t CatalogTable) hasData(o Options, excluded map[string]bool) bool {
	return t.TreeRole != "leaf" && o.DataMode != "none" && !excluded[t.Qualified()]
}

// closedExtensions is the list of extensions an archive may name (design, import step 5); an archive that names another one is invalid.
var closedExtensions = map[string]bool{
	"kingres_shard": true, "kingres_ledger": true, "kingres_columnar": true, "kingres_timepart": true, "kingres_sketch": true,
	"pgcrypto": true, "uuid-ossp": true, "citext": true, "hstore": true, "pg_trgm": true, "btree_gin": true, "btree_gist": true,
	"ltree": true, "unaccent": true, "fuzzystrmatch": true, "plpgsql": true,
}
