// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.7", "0.7", 0}, {"0.7", "0.10", -1}, {"0.10", "0.7", 1}, {"1.6", "1.6.0", 0}, {"1.6", "1.6.1", -1},
		{"2", "10", -1}, {"1.0-rc", "1.0", 1}, {"", "0", 0},
	} {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("%q vs %q: %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func codes(rs []Reason) string {
	var c []string
	for _, r := range rs {
		c = append(c, r.Code)
	}
	return strings.Join(c, ",")
}

// The table of (source, target) pairs of "Versions and refusing an incompatible archive".
func TestVersionRules(t *testing.T) {
	m := goodManifest()
	m.Format, m.CatalogVersion = Format, CatalogVersion
	ext := func(shard, citext string) map[string]string {
		return map[string]string{"kingres_shard": shard, "citext": citext}
	}
	for _, tc := range []struct {
		name string
		mod  func(*Manifest)
		t    Target
		want string
	}{
		{"same major", nil, Target{PostgresMajor: 18, Extensions: ext("0.7", "1.6")}, ""},
		{"18 loads into 19", nil, Target{PostgresMajor: 19, Extensions: ext("0.7", "1.6")}, ""},
		{"19 never into 18", func(m *Manifest) { m.Source.PostgresMajor = 19 }, Target{PostgresMajor: 18, Extensions: ext("0.7", "1.6")}, "postgres_major"},
		{"newer extension is fine", nil, Target{PostgresMajor: 18, Extensions: ext("0.10", "1.7")}, ""},
		{"older extension refused", nil, Target{PostgresMajor: 18, Extensions: ext("0.6", "1.6")}, "extension_version"},
		{"missing extension", nil, Target{PostgresMajor: 18, Extensions: map[string]string{"citext": "1.6"}}, "extension_missing"},
		{"every reason is listed", func(m *Manifest) { m.Source.PostgresMajor = 19; m.Source.Encoding = "LATIN1" }, Target{PostgresMajor: 18, Encoding: "UTF8", Extensions: ext("0.6", "1.5")}, "postgres_major,extension_version,extension_version,encoding"},
		{"locale differs", nil, Target{LcCollate: "en_US.UTF-8"}, "lc_collate"},
		{"provider differs", nil, Target{Provider: "icu"}, "locale_provider"},
		{"unchecked target", nil, Target{}, ""},
		{"unknown format", func(m *Manifest) { m.Format = "kingres-export/2" }, Target{}, "format"},
		{"unknown catalog version", func(m *Manifest) { m.CatalogVersion = 2 }, Target{}, "catalog_version"},
		{"known newer catalog version", func(m *Manifest) { m.CatalogVersion = 2 }, Target{KnownCatalogVersions: []int{1, 2}}, ""},
		{"unknown required feature", func(m *Manifest) { m.Requires = []string{"x"} }, Target{}, "requires"},
		{"known required feature", func(m *Manifest) { m.Requires = []string{"x"} }, Target{KnownFeatures: []string{"x"}}, ""},
		{"contract version is informational", func(m *Manifest) { m.Source.ContractVersion = "9.9.9" }, Target{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := m
			mm.Extensions = append([]ManifestExtension(nil), m.Extensions...)
			if tc.mod != nil {
				tc.mod(&mm)
			}
			if got := codes(CheckCompatibility(mm, tc.t)); got != tc.want {
				t.Fatalf("%q, want %q", got, tc.want)
			}
		})
	}
	c := goodCatalog()
	if rs := CheckCatalogCompatibility(c, Target{}); len(rs) != 0 {
		t.Fatal(rs)
	}
	c.DistributedSchemas = []DistributedSchema{{Schema: "tenant", Colocation: "1"}}
	if rs := CheckCatalogCompatibility(c, Target{}); codes(rs) != "distributed_schemas" {
		t.Fatal(rs)
	}
	if rs := CheckCatalogCompatibility(c, Target{SupportsDistributedSchemas: true}); len(rs) != 0 {
		t.Fatal(rs)
	}
}

func TestManifestDecoder(t *testing.T) {
	b, m := buildGood(t, nil)
	_ = b
	enc, _ := json.Marshal(m)
	if _, err := ParseManifest(enc); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"empty":        ``,
		"not json":     `x`,
		"trailing":     string(enc) + ` {}`,
		"array":        `[]`,
		"other family": `{"format":"zip/1"}`,
		"too large":    string(enc) + strings.Repeat(" ", MaxManifestBytes),
		"deep":         strings.Repeat(`{"a":`, 40) + `1` + strings.Repeat(`}`, 40),
		"long string":  `{"format":"kingres-export/1","catalogVersion":1,"x":"` + strings.Repeat("a", 5000) + `"}`,
		"many members": `{"format":"kingres-export/1","catalogVersion":1,"x":[` + strings.Repeat("0,", MaxArrayItems+1) + `0]}`,
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCatalogDecoder(t *testing.T) {
	enc, _ := json.Marshal(goodCatalog())
	c, err := ParseCatalog(enc)
	if err != nil || len(c.Tables) != 5 {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Catalog){
		"leaf without parent":         func(c *Catalog) { c.Tables[4].Parent = "" },
		"parent missing":              func(c *Catalog) { c.Tables[4].Parent = "public.zzz" },
		"policy for a missing table":  func(c *Catalog) { c.TimepartPolicies[0].Table = "zzz" },
		"unknown kind":                func(c *Catalog) { c.Tables[0].Kind = "magic" },
		"distribution column unknown": func(c *Catalog) { c.Tables[0].DistributionColumn = "nope" },
		"reference with a column":     func(c *Catalog) { c.Tables[1].DistributionColumn = "id" },
		"columnar options on heap":    func(c *Catalog) { c.Tables[0].Columnar = map[string]string{"a": "b"} },
		"duplicate table":             func(c *Catalog) { c.Tables[1] = c.Tables[0] },
		"negative estimate":           func(c *Catalog) { c.Tables[0].EstimatedRows = -1 },
		"no columns":                  func(c *Catalog) { c.Tables[0].Columns = nil },
		"bad partition method":        func(c *Catalog) { c.Tables[0].UserPartitioning = &UserPartitioning{Method: "hash", Key: "id"} },
		"control character in text":   func(c *Catalog) { c.Tables[0].Ledger = &LedgerInfo{Mode: "m", Policy: "p", HeadBlockHash: "a\x01"} },
		"sequence owner too long":     func(c *Catalog) { c.Sequences[0].OwnedBy = strings.Repeat("a", 300) },
	} {
		cc := goodCatalog()
		mod(&cc)
		b, _ := json.Marshal(cc)
		if _, err := ParseCatalog(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ParseCatalog([]byte(`{"catalogVersion":2}`)); code(err) != CodeIncompatible {
		t.Errorf("catalog version 2: %v", err)
	}
}
