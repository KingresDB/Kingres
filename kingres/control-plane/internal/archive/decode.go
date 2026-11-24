// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

var (
	hex64RE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionRE  = regexp.MustCompile(`^[0-9A-Za-z._-]{1,32}$`)
	wordRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,31}$`)
	textRE     = regexp.MustCompile(`^[\x20-\x7e]{0,128}$`)
	treeRoles  = map[string]bool{"standalone": true, "root": true, "leaf": true, "inheritanceParent": true, "inheritanceChild": true}
	tableKinds = map[string]bool{"local": true, "hash": true, "reference": true, "schema": true}
	levels     = map[string]bool{"single-snapshot": true, "per-group": true, "per-group-and-partition": true, "archive-single-snapshot": true}
)

// scanJSON checks the bounds of a JSON document before it is decoded: the nesting depth, the number of members of an array or an object and the
// length of a string (the raw bytes between the quotes). It does not check that the document is JSON; the decoder does.
func scanJSON(b []byte) error {
	depth := 0
	var counts []int
	inStr, esc := false, false
	strLen := 0
	for _, c := range b {
		if inStr {
			strLen++
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			if strLen > MaxStringBytes+1 {
				return invalid("json_limit", "", "a string of the document is longer than %d bytes", MaxStringBytes)
			}
			continue
		}
		switch c {
		case '"':
			inStr, strLen = true, 0
		case '[', '{':
			depth++
			if depth > MaxJSONDepth {
				return invalid("json_limit", "", "the document is nested deeper than %d levels", MaxJSONDepth)
			}
			counts = append(counts, 1)
		case ']', '}':
			if depth > 0 {
				depth--
				counts = counts[:len(counts)-1]
			}
		case ',':
			if len(counts) > 0 {
				counts[len(counts)-1]++
				if counts[len(counts)-1] > MaxArrayItems {
					return invalid("json_limit", "", "an array or object of the document has more than %d members", MaxArrayItems)
				}
			}
		}
	}
	return nil
}

func decodeStrict(b []byte, dst any) error {
	if err := scanJSON(b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return invalid("json", "", "the document does not have the shape of its schema")
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("json", "", "the document has data after its end")
	}
	return nil
}

func ident(object, what, s string) *Error {
	if _, err := sqlx.NewIdent(s); err != nil {
		return invalid("identifier", object, "%s is not a valid identifier", what)
	}
	return nil
}

func plain(object, what, s string) *Error {
	if !textRE.MatchString(s) {
		return invalid("text", object, "%s has characters that are not allowed", what)
	}
	return nil
}

// ParseManifest decodes and checks the structure of manifest.json. It is the decoder of the validator and the fuzz target of the manifest. A manifest
// of another format major, of another catalog version, or with a required feature that this package does not know is CodeIncompatible, and is
// reported before the rest of the document is read.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if len(b) > MaxManifestBytes {
		return m, invalid("manifest_size", ManifestName, "the manifest is larger than %d bytes", MaxManifestBytes)
	}
	if err := scanJSON(b); err != nil {
		return m, err
	}
	var head struct {
		Format         string          `json:"format"`
		CatalogVersion int             `json:"catalogVersion"`
		Requires       json.RawMessage `json:"requires"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return m, invalid("json", ManifestName, "the manifest is not a JSON document")
	}
	if major, ok := formatMajor(head.Format); !ok {
		return m, invalid("format", ManifestName, "the manifest has no format of this family")
	} else if major != FormatMajor {
		return m, &Error{Code: CodeIncompatible, Reasons: []Reason{{Code: "format", Object: ManifestName, Message: "the archive has format major " + strconv.Itoa(major) + " and this server reads " + strconv.Itoa(FormatMajor)}}}
	}
	if head.CatalogVersion != CatalogVersion {
		return m, &Error{Code: CodeIncompatible, Reasons: []Reason{{Code: "catalog_version", Object: ManifestName, Message: "the catalog version of the archive is not one this server reads"}}}
	}
	if r := bytes.TrimSpace(head.Requires); len(r) > 0 && string(r) != "null" && string(r) != "[]" {
		return m, &Error{Code: CodeIncompatible, Reasons: []Reason{{Code: "requires", Object: ManifestName, Message: "the archive requires a feature that this server does not know"}}}
	}
	if err := decodeStrict(b, &m); err != nil {
		return Manifest{}, err
	}
	if err := m.check(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func formatMajor(f string) (int, bool) {
	const p = "kingres-export/"
	if !strings.HasPrefix(f, p) {
		return 0, false
	}
	n, err := strconv.Atoi(f[len(p):])
	if err != nil || n < 0 || n > 1000 || strconv.Itoa(n) != f[len(p):] {
		return 0, false
	}
	return n, true
}

func (m *Manifest) check() *Error {
	if m.Format != Format {
		return invalid("format", ManifestName, "the format is not %s", Format)
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		return invalid("created_at", ManifestName, "createdAt is not a time")
	}
	s := m.Source
	if e := ident(ManifestName, "the source database", s.Database); e != nil {
		return e
	}
	for _, f := range []struct{ what, v string }{{"kingresVersion", s.KingresVersion}, {"contractVersion", s.ContractVersion}, {"encoding", s.Encoding}, {"lcCollate", s.LcCollate}, {"lcCtype", s.LcCtype}, {"localeProvider", s.LocaleProvider}} {
		if e := plain(ManifestName, f.what, f.v); e != nil {
			return e
		}
	}
	if s.Encoding == "" {
		return invalid("source", ManifestName, "the encoding is missing")
	}
	if s.PostgresMajor < 10 || s.PostgresMajor > 99 {
		return invalid("source", ManifestName, "postgresMajor is out of range")
	}
	if s.GroupCount < 1 || s.GroupCount > 10000 {
		return invalid("source", ManifestName, "groupCount is out of range")
	}
	if len(m.Extensions) > 100 {
		return invalid("extensions", ManifestName, "too many extensions")
	}
	seen := map[string]bool{}
	for _, x := range m.Extensions {
		if !closedExtensions[x.Name] {
			return invalid("extension_unlisted", x.Name, "an extension of the archive is not on the list of extensions that may be carried")
		}
		if seen[x.Name] {
			return invalid("extensions", x.Name, "an extension is listed twice")
		}
		seen[x.Name] = true
		if !versionRE.MatchString(x.Version) {
			return invalid("extensions", x.Name, "an extension version is not valid")
		}
		if e := ident(x.Name, "the schema of an extension", x.Schema); e != nil {
			return e
		}
	}
	if m.CatalogVersion != CatalogVersion {
		return invalid("catalog_version", ManifestName, "catalogVersion is not %d", CatalogVersion)
	}
	if !hex64RE.MatchString(m.Fingerprint) {
		return invalid("fingerprint", ManifestName, "the fingerprint is not a SHA-256")
	}
	if m.Options.DataMode != "all" && m.Options.DataMode != "none" {
		return invalid("options", ManifestName, "dataMode is not all or none")
	}
	for _, t := range m.Options.ExcludedTables {
		if !validQualified(t) {
			return invalid("options", t, "an excluded table is not a name")
		}
	}
	if len(m.Entries) > MaxEntries || len(m.Entries) < len(FixedNames) {
		return invalid("entries", ManifestName, "the number of entries is out of range")
	}
	var tables int
	var rows, bytesSum int64
	for i, e := range m.Entries {
		if e.Name != "" && len(e.Name) > MaxNameBytes {
			return invalid("entries", e.Name, "an entry name is too long")
		}
		if e.Size < 0 || !hex64RE.MatchString(e.SHA256) {
			return invalid("entries", e.Name, "an entry has no valid size or SHA-256")
		}
		if _, data := IsData(e.Name); data {
			if !validQualified(e.Table) || len(e.Columns) == 0 || len(e.Columns) > 1600 || e.Rows < 0 {
				return invalid("entries", e.Name, "a data entry has no valid table, columns or rows")
			}
			for _, c := range e.Columns {
				if ee := ident(e.Name, "a column", c); ee != nil {
					return ee
				}
			}
			if !levels[e.Consistency] || e.Groups < 1 || e.Groups > 10000 {
				return invalid("entries", e.Name, "a data entry has no valid consistency label")
			}
			if !e.LedgerHistory {
				tables++
				rows += e.Rows
			}
		} else {
			if e.Table != "" || len(e.Columns) != 0 || e.Rows != 0 || e.Consistency != "" || e.Groups != 0 || e.LedgerHistory {
				return invalid("entries", e.Name, "an entry that is not a data entry has members of one")
			}
			if e.Name != CatalogName && !IsSQL(e.Name) {
				return invalid("entries", e.Name, "the manifest names an entry that an archive does not have")
			}
		}
		_ = i
		bytesSum += e.Size
		if bytesSum < 0 {
			return invalid("entries", ManifestName, "the sizes overflow")
		}
	}
	if m.Totals.Tables != tables || m.Totals.Rows != rows || m.Totals.UncompressedBytes != bytesSum {
		return invalid("totals", ManifestName, "the totals do not add up to the entries")
	}
	return nil
}

func validQualified(q string) bool {
	i := strings.IndexByte(q, '.')
	if i <= 0 {
		return false
	}
	_, e1 := sqlx.NewIdent(q[:i])
	_, e2 := sqlx.NewIdent(q[i+1:])
	return e1 == nil && e2 == nil
}

// ParseCatalog decodes and checks catalog.json: the identifiers, the closed words, the bucket counts and the references between its parts. It is the
// decoder of the validator and the fuzz target of the catalog.
func ParseCatalog(b []byte) (Catalog, error) {
	var c Catalog
	if len(b) > MaxCatalogBytes {
		return c, invalid("catalog_size", CatalogName, "the catalog is larger than %d bytes", MaxCatalogBytes)
	}
	if err := decodeStrict(b, &c); err != nil {
		return Catalog{}, err
	}
	if err := c.check(); err != nil {
		return Catalog{}, err
	}
	return c, nil
}

func (c *Catalog) check() *Error {
	if c.CatalogVersion != CatalogVersion {
		return &Error{Code: CodeIncompatible, Reasons: []Reason{{Code: "catalog_version", Object: CatalogName, Message: "the catalog version is not one this server reads"}}}
	}
	groups := map[string]bool{}
	for _, g := range c.ColocationGroups {
		if e := ident(CatalogName, "a colocation reference", g.Ref); e != nil {
			return e
		}
		if g.BucketCount < 1 || g.BucketCount > MaxBuckets {
			return invalid("bucket_count", g.Ref, "a bucket count is not from 1 to %d", MaxBuckets)
		}
		if groups[g.Ref] {
			return invalid("colocation", g.Ref, "a colocation reference is listed twice")
		}
		groups[g.Ref] = true
		if e := plain(g.Ref, "the type of a distribution column", g.DistColumnType); e != nil {
			return e
		}
		if e := plain(g.Ref, "a collation", g.Collation); e != nil {
			return e
		}
	}
	if len(c.Tables) > 10000 {
		return invalid("tables", CatalogName, "more than 10000 tables")
	}
	byName := map[string]*CatalogTable{}
	for i := range c.Tables {
		t := &c.Tables[i]
		q := t.Qualified()
		if e := ident(CatalogName, "a schema", t.Schema); e != nil {
			return e
		}
		if e := ident(CatalogName, "a table", t.Name); e != nil {
			return e
		}
		switch t.Schema {
		case "kingres", "pg_catalog", "information_schema", "pg_toast":
			return invalid("system_schema", t.Schema, "a table is in a schema of the system")
		}
		// A name is written "schema.table" in the manifest and the catalog; a dot in either part would make two tables the same text.
		if strings.Contains(t.Schema, ".") || strings.Contains(t.Name, ".") {
			return invalid("tables", q, "a schema or a table has a dot in its name")
		}
		if byName[q] != nil {
			return invalid("tables", q, "a table is listed twice")
		}
		byName[q] = t
		if !tableKinds[t.Kind] || !treeRoles[t.TreeRole] {
			return invalid("tables", q, "a table has a kind or a tree role that is not known")
		}
		if t.AccessMethod != "heap" && t.AccessMethod != "columnar" {
			return invalid("tables", q, "a table has an access method that is not known")
		}
		if len(t.Columns) == 0 || len(t.Columns) > 1600 {
			return invalid("tables", q, "a table has no columns or too many")
		}
		for _, col := range t.Columns {
			if e := ident(q, "a column", col); e != nil {
				return e
			}
		}
		switch t.Kind {
		case "hash":
			if t.DistributionColumn == "" || !contains(t.Columns, t.DistributionColumn) || !groups[t.ColocationRef] {
				return invalid("tables", q, "a hash table has no distribution column of its own or no colocation group of the catalog")
			}
		case "reference", "local":
			if t.DistributionColumn != "" || t.ColocationRef != "" {
				return invalid("tables", q, "a table that is not hash-distributed has a distribution")
			}
		case "schema":
			if t.DistributionColumn != "" {
				return invalid("tables", q, "a table of a distributed schema has a distribution column")
			}
		}
		if t.UserPartitioning != nil {
			up := t.UserPartitioning
			if up.Method != "range" && up.Method != "list" || len(up.Partitions) > 10000 {
				return invalid("tables", q, "the user partitioning of a table is not valid")
			}
			if e := plain(q, "a partition key", up.Key); e != nil || up.Key == "" {
				return invalid("tables", q, "a partition key is not valid")
			}
			for _, p := range up.Partitions {
				if e := ident(q, "a partition", p.Name); e != nil {
					return e
				}
				if e := plain(q, "a partition bound", p.Bound); e != nil {
					return e
				}
			}
		}
		for k, v := range t.Columnar {
			if !wordRE.MatchString(k) || !textRE.MatchString(v) {
				return invalid("tables", q, "a columnar option is not valid")
			}
		}
		if t.Columnar != nil && t.AccessMethod != "columnar" {
			return invalid("tables", q, "a table that is not columnar has columnar options")
		}
		if l := t.Ledger; l != nil {
			if !wordRE.MatchString(l.Mode) || !wordRE.MatchString(l.Policy) || l.HeadHeight < 0 || len(l.HeadBlockHash) > 128 || !textRE.MatchString(l.HeadBlockHash) {
				return invalid("tables", q, "the ledger of a table is not valid")
			}
			for _, col := range l.HistoryColumns {
				if e := ident(q, "a history column", col); e != nil {
					return e
				}
			}
		}
		if t.Parent != "" && !validQualified(t.Parent) {
			return invalid("tables", q, "the parent of a table is not a name")
		}
		if (t.TreeRole == "leaf" || t.TreeRole == "inheritanceChild") && t.Parent == "" {
			return invalid("tables", q, "a leaf or a child has no parent")
		}
		if t.EstimatedRows < 0 || t.EstimatedBytes < 0 {
			return invalid("tables", q, "an estimate is negative")
		}
	}
	for _, t := range c.Tables {
		if t.Parent != "" && byName[t.Parent] == nil {
			return invalid("tables", t.Qualified(), "the parent of a table is not in the catalog")
		}
	}
	if len(c.DistributedSchemas) > 10000 || len(c.TimepartPolicies) > 10000 || len(c.Sequences) > 100000 {
		return invalid("catalog", CatalogName, "a list of the catalog is too long")
	}
	for _, d := range c.DistributedSchemas {
		if e := ident(CatalogName, "a distributed schema", d.Schema); e != nil {
			return e
		}
		if e := plain(d.Schema, "a colocation position", d.Colocation); e != nil {
			return e
		}
	}
	for _, p := range c.TimepartPolicies {
		if byName[p.Schema+"."+p.Table] == nil {
			return invalid("timepart", p.Table, "a policy is for a table that is not in the catalog")
		}
		for _, v := range []string{p.PartitionEvery, p.Timezone, p.CreateAhead, p.Retention, p.RetentionAction, p.ColumnarAfter, p.ColumnarSort, p.RunEvery} {
			if e := plain(p.Table, "a policy member", v); e != nil {
				return e
			}
		}
		for k, v := range p.ColumnarOptions {
			if !wordRE.MatchString(k) || !textRE.MatchString(v) {
				return invalid("timepart", p.Table, "a columnar option of a policy is not valid")
			}
		}
	}
	for _, s := range c.Sequences {
		if e := ident(CatalogName, "a sequence schema", s.Schema); e != nil {
			return e
		}
		if e := ident(CatalogName, "a sequence", s.Name); e != nil {
			return e
		}
		if len(s.OwnedBy) > 200 || hasControl(s.OwnedBy) {
			return invalid("sequences", s.Name, "ownedBy is not a name")
		}
	}
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
