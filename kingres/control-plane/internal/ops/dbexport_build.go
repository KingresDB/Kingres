// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// The documents of a database archive that are made from the facts: the catalog document, the facts of the normaliser, the consistency labels and the
// manifest (contract 0.40.0, design "The catalog document" and "The archive").

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/dumpnorm"
)

// ContractRevision is the revision of the contract that the archives written by this build name in their manifest.
const ContractRevision = "0.40.0"

// isBucket reports whether a table exists only because of the distribution (a bucket partition of a hash table or a leaf under one).
func (f *dbFacts) isBucket(t dbTable) bool { return f.Buckets[t.OID] }

// isHistory reports whether the table is the history table of a ledger table.
func (f *dbFacts) historyOf() map[int64]bool {
	h := map[int64]bool{}
	for _, t := range f.Tables {
		if t.Ledger != nil && t.Ledger.HistoryOID != 0 {
			h[t.Ledger.HistoryOID] = true
		}
	}
	return h
}

// treeRole is the role of a table in a tree (design "Partition trees and inheritance").
func (f *dbFacts) treeRole(t dbTable) string {
	switch {
	case t.Method == "hash" && len(f.UserPart[t.OID]) > 0:
		return "root"
	case t.Method == "hash":
		return "standalone"
	case t.IsPartition && f.byOID[t.Parent] != nil:
		return "leaf"
	case t.Relkind == "p":
		return "root"
	case t.Children > 0:
		return "inheritanceParent"
	case len(t.Inherits) > 0:
		return "inheritanceChild"
	}
	return "standalone"
}

// refs names the colocation groups that the tables use, in the order of their numbers: c1, c2, ...
func (f *dbFacts) refs() map[int]string {
	used := map[int]bool{}
	for _, t := range f.Tables {
		if t.Method == "hash" {
			used[t.Coloc] = true
		}
	}
	ids := make([]int, 0, len(used))
	for id := range used {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := map[int]string{}
	for i, id := range ids {
		out[id] = fmt.Sprintf("c%d", i+1)
	}
	return out
}

func splitPartKey(def string) (method, key string) {
	i := strings.Index(def, " (")
	if i < 0 || !strings.HasSuffix(def, ")") {
		return "", ""
	}
	return strings.ToLower(def[:i]), def[i+2 : len(def)-1]
}

// dataTables are the tables of the archive, in the order of the catalog: the user tables that are not bucket partitions or history tables.
func (f *dbFacts) dataTables() []dbTable {
	hist := f.historyOf()
	var out []dbTable
	for _, t := range f.Tables {
		if f.isBucket(t) || hist[t.OID] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// catalog makes catalog.json.
func (f *dbFacts) catalog() archive.Catalog {
	refs := f.refs()
	c := archive.Catalog{CatalogVersion: archive.CatalogVersion, ColocationGroups: []archive.ColocationGroup{}, Tables: []archive.CatalogTable{}, TimepartPolicies: []archive.TimepartPolicy{}, Sequences: []archive.CatalogSequence{}}
	ids := make([]int, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		g := f.Colocs[id]
		if g == nil {
			continue
		}
		c.ColocationGroups = append(c.ColocationGroups, archive.ColocationGroup{Ref: refs[id], BucketCount: g.Buckets, DistColumnType: g.Type, Collation: g.Collation})
	}
	inTable := map[string]bool{}
	for _, t := range f.dataTables() {
		ct := archive.CatalogTable{Schema: t.Schema, Name: t.Name, Kind: "local", TreeRole: f.treeRole(t), Columns: append([]string{}, t.Cols...), AccessMethod: t.AM,
			EstimatedRows: t.Rows, EstimatedBytes: t.Bytes}
		if ct.AccessMethod == "" || t.Relkind == "p" {
			ct.AccessMethod = "heap"
		}
		if p := f.byOID[t.Parent]; p != nil && ct.TreeRole == "leaf" {
			ct.Parent = p.qualified()
		}
		if len(t.Inherits) > 0 && ct.TreeRole != "leaf" {
			if p := f.byOID[t.Inherits[0]]; p != nil {
				ct.Parent = p.qualified()
			}
		}
		switch t.Method {
		case "hash":
			ct.Kind, ct.DistributionColumn, ct.ColocationRef = "hash", t.DistColumn, refs[t.Coloc]
			if parts := f.UserPart[t.OID]; len(parts) > 0 {
				m, k := splitPartKey(f.PartKey[t.OID])
				up := &archive.UserPartitioning{Method: m, Key: k}
				for _, p := range parts {
					up.Partitions = append(up.Partitions, archive.UserPartition{Name: p.Name, Bound: p.Bound})
				}
				ct.UserPartitioning = up
			}
		case "reference":
			ct.Kind = "reference"
		case "schema":
			ct.Kind = "schema"
		}
		if o, ok := f.columnar[t.OID]; ok {
			ct.Columnar = o
		}
		if t.Ledger != nil {
			ct.Ledger = &archive.LedgerInfo{Mode: t.Ledger.Mode, Policy: t.Ledger.Policy, HeadHeight: t.Ledger.HeadHeight, HeadBlockHash: t.Ledger.HeadHash, HistoryColumns: t.Ledger.HistoryCols}
		}
		c.Tables = append(c.Tables, ct)
		inTable[ct.Qualified()] = true
	}
	for _, p := range f.Policies {
		if !inTable[p.Schema+"."+p.Table] {
			continue
		}
		c.TimepartPolicies = append(c.TimepartPolicies, archive.TimepartPolicy{Schema: p.Schema, Table: p.Table, PartitionEvery: p.Every, Timezone: p.Zone, CreateAhead: p.Ahead,
			Retention: p.Retention, RetentionAction: p.Action, ColumnarAfter: p.ColAfter, ColumnarSort: p.ColSort, ColumnarOptions: p.ColOpts, RunEvery: p.Run, Enabled: p.Enabled})
	}
	for _, s := range f.Seqs {
		owner := s.Owned
		if i := strings.LastIndex(owner, "."); i > 0 && !inTable[owner[:i]] {
			continue
		}
		c.Sequences = append(c.Sequences, archive.CatalogSequence{Schema: s.Schema, Name: s.Name, OwnedBy: s.Owned, LastValue: s.Last, IsCalled: s.Called})
	}
	for _, s := range f.Schemas {
		c.DistributedSchemas = append(c.DistributedSchemas, archive.DistributedSchema{Schema: s.Name, Colocation: fmt.Sprintf("s%d", s.Coloc)})
	}
	return c
}

// normFacts makes the facts of the normaliser.
func (f *dbFacts) normFacts() dumpnorm.Facts {
	nf := dumpnorm.Facts{Remove: map[string]bool{}, RemoveIndex: map[string]bool{}, Hash: map[string]dumpnorm.HashTable{}, Ledger: map[string]bool{}}
	for _, n := range f.BucketNames {
		nf.Remove[n] = true
	}
	for _, n := range f.BucketIdx {
		nf.RemoveIndex[n] = true
	}
	for _, t := range f.Tables {
		if t.Ledger != nil {
			nf.Ledger[t.qualified()] = true
			if t.Ledger.HistoryName != "" {
				nf.Ledger[t.Ledger.HistoryName] = true
				nf.Remove[t.Ledger.HistoryName] = true
			}
		}
		if t.Method != "hash" {
			continue
		}
		ht := dumpnorm.HashTable{Column: t.DistColumn}
		if parts := f.UserPart[t.OID]; len(parts) > 0 {
			m, k := splitPartKey(f.PartKey[t.OID])
			up := &dumpnorm.UserPartitioning{Method: strings.ToUpper(m), Key: k}
			for _, p := range parts {
				up.Partitions = append(up.Partitions, dumpnorm.Leaf{Name: p.Name, Bound: p.Bound})
			}
			ht.User = up
		}
		nf.Hash[t.qualified()] = ht
	}
	return nf
}

// inheritanceChildren are the tables that may carry INHERITS in the schema files.
func (f *dbFacts) inheritanceChildren() map[string]bool {
	out := map[string]bool{}
	for _, t := range f.Tables {
		if len(t.Inherits) > 0 {
			out[t.qualified()] = true
		}
	}
	return out
}

// label is the consistency label of one table when each table is read by a statement of its own (design "What is consistent").
func (f *dbFacts) label(t dbTable) (level string, groups int) {
	groups = 1
	level = "single-snapshot"
	if t.Method == "hash" {
		if g := f.Colocs[t.Coloc]; g != nil && g.Groups > 1 {
			level, groups = "per-group", g.Groups
		}
	}
	return level, groups
}

// singleSnapshot reports whether every table can be read in one transaction that is one snapshot: no table has buckets in more than one group.
func (f *dbFacts) singleSnapshot() bool {
	for _, t := range f.dataTables() {
		if l, _ := f.label(t); l != "single-snapshot" {
			return false
		}
	}
	return true
}

// groupCount is the largest number of groups that hold buckets of a table of the database.
func (f *dbFacts) groupCount() int {
	n := 1
	for _, g := range f.Colocs {
		if g.Groups > n {
			n = g.Groups
		}
	}
	return n
}

// dataEntry is a table that is written to an entry of the archive.
type dataEntry struct {
	Table   dbTable
	Cols    []string
	Only    bool
	History bool
	// Base is the ledger table whose history this entry is (the manifest names that table).
	Base string
}

// dataEntries are the data entries of an archive with these options, in the order of the catalog; the history of an updatable ledger table follows its table.
func (f *dbFacts) dataEntries(excluded map[string]bool, dataMode string) []dataEntry {
	if dataMode == "none" {
		return nil
	}
	var out []dataEntry
	for _, t := range f.dataTables() {
		if f.treeRole(t) == "leaf" || excluded[t.qualified()] {
			continue
		}
		out = append(out, dataEntry{Table: t, Cols: t.Cols, Only: t.Children > 0})
		if t.Ledger != nil && t.Ledger.HistoryOID != 0 && len(t.Ledger.HistoryCols) > 0 {
			h := f.byOID[t.Ledger.HistoryOID]
			out = append(out, dataEntry{Table: *h, Cols: t.Ledger.HistoryCols, History: true, Base: t.qualified()})
		}
	}
	return out
}

// warnings of the manifest: what the archive does not carry and how it was read.
func (f *dbFacts) warnings(entries []dataEntry, single bool, ackLO bool, excluded map[string]bool) []string {
	w := []string{"roles, owners, privileges, tablespaces and the placement of buckets are not carried; an import makes the importing role the owner of every object"}
	if ackLO && (f.LargeObjects > 0 || len(f.OIDColumns) > 0) {
		w = append(w, fmt.Sprintf("large objects and oid or reg* columns are not carried: %d large objects and %d such columns were acknowledged as lost", f.LargeObjects, len(f.OIDColumns)))
	}
	if single {
		w = append(w, "every table was read in one transaction (one snapshot)")
	} else {
		for _, e := range entries {
			if e.History {
				continue
			}
			if l, g := f.label(e.Table); l == "per-group" {
				w = append(w, fmt.Sprintf("table %s was read per group (%d groups), not as one snapshot", e.Table.qualified(), g))
			}
		}
	}
	hasLedger := false
	for _, t := range f.Tables {
		if t.Ledger != nil {
			hasLedger = true
		}
	}
	if hasLedger {
		w = append(w, "ledger tables are carried as data; an import starts a new chain and the history of an updatable ledger table is carried for the record only")
	}
	if len(f.Policies) > 0 {
		w = append(w, "time-partition policies are carried and are imported disabled")
	}
	var ex []string
	for q := range excluded {
		ex = append(ex, q)
	}
	sort.Strings(ex)
	if len(ex) > 0 {
		w = append(w, fmt.Sprintf("the data of %d tables was left out at the request", len(ex)))
	}
	return w
}

// manifest makes the part of manifest.json that the exporter knows before the entries are written.
func (f *dbFacts) manifest(db, version, fingerprint string, now time.Time, dataMode string, excluded map[string]bool, warnings []string) archive.Manifest {
	m := archive.Manifest{Format: archive.Format, CreatedAt: now.UTC().Format(time.RFC3339), CatalogVersion: archive.CatalogVersion, Fingerprint: fingerprint, Warnings: warnings}
	m.Source = archive.Source{Database: db, KingresVersion: version, ContractVersion: ContractRevision, PostgresMajor: f.PGMajor, GroupCount: f.groupCount(), Encoding: f.Encoding,
		LcCollate: f.Collate, LcCtype: f.Ctype, LocaleProvider: f.Provider}
	m.Extensions = append([]archive.ManifestExtension{}, f.Extensions...)
	m.Options = archive.Options{DataMode: dataMode}
	for q := range excluded {
		m.Options.ExcludedTables = append(m.Options.ExcludedTables, q)
	}
	sort.Strings(m.Options.ExcludedTables)
	return m
}
