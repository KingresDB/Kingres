// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package pgconfig

import (
	"math/big"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// The warning codes of a preview (design config-phase13.md, "Validation" and "Security"). A warning
// never makes a request invalid: Valid is a function of the problems only.
const (
	WarnValueUnusual         = "config_value_unusual"
	WarnReservedConnections  = "config_reserved_connections"
	unusualFactor            = 10
	reservedConnectionsFloor = 1
)

func warn(i int, code, msg string) api.ConfigWarning {
	return api.ConfigWarning{Index: i, Pointer: "/changes/" + strconv.Itoa(i) + "/value", Code: code, Message: msg}
}

// isMemory says whether a parameter is a memory size: an integer in bytes, kilobytes or 8 kB pages.
func isMemory(s Setting) bool {
	if s.VarType != "integer" {
		return false
	}
	switch s.Unit {
	case "B", "kB", "8kB":
		return true
	}
	return false
}

// unusualWarning flags a memory value more than ten times the default of the parameter, or below a
// tenth of it. Exactly ten times and exactly a tenth are not flagged. A default of zero and a value
// that is not an integer in the base unit are never flagged.
func unusualWarning(i int, s Setting, r Result) (api.ConfigWarning, bool) {
	if !isMemory(s) || !r.BaseOK || r.Base == nil {
		return api.ConfigWarning{}, false
	}
	boot, ok := ratOf(s.BootVal)
	if !ok || boot.Sign() <= 0 {
		return api.ConfigWarning{}, false
	}
	factor := new(big.Rat).SetInt64(unusualFactor)
	if r.Base.Cmp(new(big.Rat).Mul(boot, factor)) > 0 {
		return warn(i, WarnValueUnusual, s.Name+" is more than ten times its default ("+Format(s, s.BootVal)+"); this is unusual."), true
	}
	if r.Base.Cmp(new(big.Rat).Quo(boot, factor)) < 0 {
		return warn(i, WarnValueUnusual, s.Name+" is below a tenth of its default ("+Format(s, s.BootVal)+"); this is unusual."), true
	}
	return api.ConfigWarning{}, false
}

var reservedNames = []string{"max_connections", "superuser_reserved_connections", "reserved_connections"}

func isReservedName(name string) bool {
	for _, n := range reservedNames {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// effectiveInt is the value of an integer parameter on a server after the change: the value that
// decides there, else the running one.
func effectiveInt(sn *snapshot, after []valueRow, s srv, name string) (int64, bool) {
	meta, ok := sn.setting(s.ID, name)
	if !ok {
		return 0, false
	}
	text := meta.Setting
	var ovs []valueRow
	for _, v := range after {
		if v.Name == name {
			ovs = append(ovs, v)
		}
	}
	if o, has := Pick(toOverrides(ovs), s.ID, s.Group); has {
		text = o.Value
	}
	n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	return n, err == nil
}

// reservedWarnings reports, for the first server where it happens, that max_connections would not
// exceed superuser_reserved_connections + reserved_connections: PostgreSQL refuses to start then. It
// is attached to the last change of the request that touches one of the three parameters.
func reservedWarnings(sn *snapshot, after []valueRow, cs []change, indexes []int) []api.ConfigWarning {
	at := -1
	for k, c := range cs {
		if isReservedName(c.Name) {
			at = indexes[k]
		}
	}
	if at < 0 {
		return nil
	}
	for _, s := range sn.servers {
		max, ok1 := effectiveInt(sn, after, s, "max_connections")
		su, ok2 := effectiveInt(sn, after, s, "superuser_reserved_connections")
		rs, ok3 := effectiveInt(sn, after, s, "reserved_connections")
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		if max < su+rs+reservedConnectionsFloor {
			return []api.ConfigWarning{warn(at, WarnReservedConnections, "On server "+s.ID+" max_connections ("+strconv.FormatInt(max, 10)+") would be below superuser_reserved_connections ("+strconv.FormatInt(su, 10)+") plus reserved_connections ("+strconv.FormatInt(rs, 10)+") plus 1; PostgreSQL would not start.")}
		}
	}
	return nil
}
