// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// Target is what the cluster that would take the archive can do. A field that is empty (or zero) is not checked.
type Target struct {
	PostgresMajor int
	// Extensions maps the name of an extension to the version that can be installed (or is installed) here; an extension that is not in the map cannot
	// be installed.
	Extensions map[string]string
	// Encoding, LcCollate, LcCtype and Provider are those the target creates databases with; the archive's must be equal.
	Encoding  string
	LcCollate string
	LcCtype   string
	Provider  string
	// KnownFormatMajors and KnownCatalogVersions default to {FormatMajor} and {CatalogVersion}. KnownFeatures are the names of `requires` that the importer knows.
	KnownFormatMajors    []int
	KnownCatalogVersions []int
	KnownFeatures        []string
	// SupportsDistributedSchemas is true for an importer of phase 11 and later.
	SupportsDistributedSchemas bool
}

// CompareVersions compares two dotted versions part by part, numerically where both parts are numbers; it returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	pa, pb := splitVersion(a), splitVersion(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		if x == "" {
			nx, ex = 0, nil
		}
		if y == "" {
			ny, ey = 0, nil
		}
		switch {
		case ex == nil && ey == nil:
			if nx != ny {
				return cmp(nx, ny)
			}
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return 0
}

func cmp(a, b int) int {
	if a < b {
		return -1
	}
	return 1
}

func splitVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
}

func inInts(l []int, d int, v int) bool {
	if len(l) == 0 {
		return v == d
	}
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// CheckCompatibility applies the table "Versions and refusing an incompatible archive" to the manifest. It returns every reason, in a fixed order; the
// archive may be loaded when there is none. Contract versions are informational and never refused.
func CheckCompatibility(m Manifest, t Target) []Reason {
	var rs []Reason
	add := func(code, object, msg string) { rs = append(rs, Reason{Code: code, Object: object, Message: msg}) }
	if major, ok := formatMajor(m.Format); !ok || !inInts(t.KnownFormatMajors, FormatMajor, major) {
		add("format", ManifestName, "the format of the archive is not one this server reads")
	}
	if !inInts(t.KnownCatalogVersions, CatalogVersion, m.CatalogVersion) {
		add("catalog_version", ManifestName, "the catalog version of the archive is not one this server reads")
	}
	for _, f := range m.Requires {
		if !contains(t.KnownFeatures, f) {
			add("requires", safe(f), "the archive requires a feature that this server does not know")
		}
	}
	if t.PostgresMajor != 0 && t.PostgresMajor < m.Source.PostgresMajor { // a dump of 18 loads into 19, never the other way
		add("postgres_major", "postgres", "the archive was made by PostgreSQL "+strconv.Itoa(m.Source.PostgresMajor)+" and the target is "+strconv.Itoa(t.PostgresMajor)+": an archive loads only into the same or a later major")
	}
	if t.Extensions != nil {
		xs := append([]ManifestExtension(nil), m.Extensions...)
		sort.Slice(xs, func(i, j int) bool { return xs[i].Name < xs[j].Name })
		for _, x := range xs {
			have, ok := t.Extensions[x.Name]
			switch {
			case !ok:
				add("extension_missing", x.Name, "the extension "+safe(x.Name)+" cannot be installed on the target")
			case CompareVersions(have, x.Version) < 0:
				add("extension_version", x.Name, "the target has "+safe(x.Name)+" "+safe(have)+" and the archive needs "+safe(x.Version)+" or later")
			}
		}
	}
	for _, f := range []struct{ what, source, target string }{
		{"encoding", m.Source.Encoding, t.Encoding}, {"lc_collate", m.Source.LcCollate, t.LcCollate},
		{"lc_ctype", m.Source.LcCtype, t.LcCtype}, {"locale_provider", m.Source.LocaleProvider, t.Provider},
	} {
		if f.target != "" && f.source != f.target {
			add(f.what, f.what, "the target cannot create a database with the "+f.what+" of the archive")
		}
	}
	return rs
}

// CheckCatalogCompatibility is the part of the table that needs the catalog: distributed schemas.
func CheckCatalogCompatibility(c Catalog, t Target) []Reason {
	if len(c.DistributedSchemas) > 0 && !t.SupportsDistributedSchemas {
		return []Reason{{Code: "distributed_schemas", Object: CatalogName, Message: "the archive has distributed schemas and this server cannot import them"}}
	}
	return nil
}

// Blockers converts reasons to the TableBlocker of the contract.
func Blockers(rs []Reason) []api.TableBlocker {
	out := make([]api.TableBlocker, 0, len(rs))
	for _, r := range rs {
		out = append(out, api.TableBlocker{Code: r.Code, Object: r.Object, Message: r.Message})
	}
	return out
}

// BuildReport makes the archive report of the contract from a validation and the reasons of the compatibility check. The archive is valid when it has no
// reason and the grammar refused nothing. Nothing in it is a value of the data or the text of a statement.
func BuildReport(res *Result, reasons []Reason, t Target) api.ArchiveReport {
	m := res.Manifest
	rep := api.ArchiveReport{Format: m.Format, SHA256: res.FileSHA256, Reasons: Blockers(reasons)}
	rep.Versions = api.ArchiveVersions{PostgresMajor: m.Source.PostgresMajor, KingresVersion: m.Source.KingresVersion, Encoding: m.Source.Encoding,
		LcCollate: m.Source.LcCollate, LcCtype: m.Source.LcCtype, Extensions: []api.ArchiveExtension{}}
	for _, x := range m.Extensions {
		ae := api.ArchiveExtension{Name: x.Name, Version: x.Version, Schema: x.Schema}
		if v, ok := t.Extensions[x.Name]; ok {
			vv := v
			ae.Installed = &vv
		}
		rep.Versions.Extensions = append(rep.Versions.Extensions, ae)
	}
	for _, ct := range res.Catalog.Tables {
		if ct.TreeRole == "leaf" {
			continue
		}
		switch ct.Kind {
		case "hash":
			rep.Tables.Hash++
		case "reference":
			rep.Tables.Reference++
		default:
			rep.Tables.Local++
		}
		if ct.Ledger != nil {
			rep.Tables.Ledger++
		}
		if ct.AccessMethod == "columnar" {
			rep.Tables.Columnar++
		}
	}
	rep.Tables.Rows, rep.Tables.Bytes = m.Totals.Rows, m.Totals.UncompressedBytes
	st := &api.ArchiveStatements{ByKind: map[string]int{}, Refused: []api.RefusedStatement{}}
	names := make([]string, 0, len(res.SQL))
	for n := range res.SQL {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sr := res.SQL[n]
		for k, v := range sr.ByKind {
			st.ByKind[k] += v
		}
		for _, r := range sr.Refused {
			st.Refused = append(st.Refused, api.RefusedStatement{Kind: r.Kind, Position: r.Position, Fingerprint: r.Fingerprint, Reason: r.Reason})
		}
	}
	if len(res.SQL) > 0 {
		rep.Statements = st
	}
	rep.Needs = &api.ArchiveNeeds{LedgerRestart: rep.Tables.Ledger > 0, LargeObjectsLost: hasWarning(m.Warnings, "large objects")}
	rep.Warnings = append([]string{}, m.Warnings...)
	rep.Reasons = Blockers(reasons)
	rep.Valid = len(reasons) == 0 && len(st.Refused) == 0
	return rep
}

func hasWarning(ws []string, needle string) bool {
	for _, w := range ws {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}

// ReportFromError makes the report of an archive that did not validate: not valid, with the reasons of the error.
func ReportFromError(err error, fileSHA string) api.ArchiveReport {
	rep := api.ArchiveReport{SHA256: fileSHA, Reasons: []api.TableBlocker{}, Versions: api.ArchiveVersions{Extensions: []api.ArchiveExtension{}}}
	var ae *Error
	if errors.As(err, &ae) {
		rep.Reasons = Blockers(ae.Reasons)
	} else {
		rep.Reasons = []api.TableBlocker{{Code: "unreadable", Object: "", Message: "the archive could not be read"}}
	}
	return rep
}
