// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import "testing"

// M1 of the review of g2: the schemas of KingresDB are its own. An extension of the list that is not one of KingresDB's is refused in kingres and in kingres_*; an
// extension of KingresDB may be in kingres only.
func TestManifestRefusesAnExtensionInAReservedSchema(t *testing.T) {
	for _, c := range []struct {
		ext, schema string
		refused     bool
	}{
		{"pgcrypto", "kingres", true}, {"ltree", "kingres_transfer", true}, {"hstore", "Kingres_X", true}, {"kingres_ledger", "kingres_x", true},
		{"kingres_ledger", "kingres", false}, {"pgcrypto", "public", false}, {"pgcrypto", "ext", false}, {"kingres_columnar", "pg_catalog", false},
	} {
		m := goodManifest()
		m.Format = Format
		m.Extensions = append(m.Extensions, ManifestExtension{Name: c.ext, Version: "1.0", Schema: c.schema})
		err := m.check()
		got := false
		if err != nil {
			for _, r := range err.Reasons {
				got = got || r.Code == "extension_schema"
			}
		}
		if got != c.refused {
			t.Errorf("%s in %s: refused %v (%v), want %v", c.ext, c.schema, got, err, c.refused)
		}
	}
}

// The catalog says which tables are distributed; a manifest without kingres_shard cannot go with it.
func TestCatalogThatNeedsShardingNeedsTheExtensionInTheManifest(t *testing.T) {
	cat := goodCatalog()
	if !cat.NeedsShard() {
		t.Fatal("a catalog with a hash table does not need the sharding extension")
	}
	byTable := map[string]*CatalogTable{}
	for i := range cat.Tables {
		byTable[cat.Tables[i].Qualified()] = &cat.Tables[i]
	}
	m := goodManifest()
	m.CatalogVersion = cat.CatalogVersion
	var keep []ManifestExtension
	for _, x := range m.Extensions {
		if x.Name != "kingres_shard" {
			keep = append(keep, x)
		}
	}
	m.Extensions = keep
	if err := coherent(m, cat, byTable); err == nil || code(err) != "archive_invalid" || len(err.(*Error).Reasons) == 0 || err.(*Error).Reasons[0].Code != "extension_missing" {
		t.Errorf("a catalog with distributed tables and a manifest without kingres_shard: %v", err)
	}
	local := Catalog{CatalogVersion: 1, Tables: []CatalogTable{{Schema: "public", Name: "t", Kind: "local", TreeRole: "standalone", Columns: []string{"a"}, AccessMethod: "heap"}}}
	if local.NeedsShard() {
		t.Error("a catalog of local tables needs the sharding extension")
	}
	if !(Catalog{DistributedSchemas: []DistributedSchema{{Schema: "s"}}}).NeedsShard() || !(Catalog{ColocationGroups: []ColocationGroup{{Ref: "g"}}}).NeedsShard() {
		t.Error("a distributed schema or a colocation group does not need the sharding extension")
	}
}
