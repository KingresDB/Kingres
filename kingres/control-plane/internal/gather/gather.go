// SPDX-License-Identifier: PostgreSQL

// Package gather answers the cluster-wide reads (logs, alerts, metrics) from
// any server: the receiving server asks every other server of the cluster in
// parallel, each with its own deadline, merges what came back with its own part
// and says which servers did not answer. This package has the pieces that do not
// depend on the network or on HTTP: the parallel call with deadlines, the total
// orders and keyset cursors that keep paging stable across servers, and the
// merge. The answering side and the wire are in package httpapi and in
// internal-api.md.
package gather

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// Peer is a server that is asked: its id and its ordinal, the number that makes
// the ids of its alerts unique in the cluster (see AlertID).
type Peer struct {
	ID      string
	Ordinal int
}

// Why a server did not answer (the values of MissingServer.reason of the
// contract).
const (
	ReasonTimeout     = "timeout"
	ReasonUnreachable = "unreachable"
	ReasonRefused     = "refused"
	ReasonInvalid     = "invalidAnswer"
)

// The errors a caller reports.
var (
	// ErrUnreachable: the server could not be reached.
	ErrUnreachable = errors.New("gather: the server cannot be reached")
	// ErrRefused: the server answered with an error status.
	ErrRefused = errors.New("gather: the server refused the request")
	// ErrInvalid: the answer was too large or not what was asked for.
	ErrInvalid = errors.New("gather: the answer is not valid")
)

// ReasonOf maps an error to the reason reported to the client.
func ReasonOf(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.Is(err, ErrRefused):
		return ReasonRefused
	case errors.Is(err, ErrInvalid):
		return ReasonInvalid
	}
	return ReasonUnreachable
}

// Outcome is the answer of one server (Body) or why there is none (Err).
type Outcome struct {
	Peer Peer
	Body []byte
	Err  error
}

// Missing is a server that did not answer.
type Missing struct {
	ServerID string
	Reason   string
}

// String is for diagnostics.
func (m Missing) String() string { return fmt.Sprintf("%s (%s)", m.ServerID, m.Reason) }

// MissingOf lists the servers without an answer, sorted by server id.
func MissingOf(out []Outcome) []Missing {
	var m []Missing
	for _, o := range out {
		if o.Err != nil {
			m = append(m, Missing{o.Peer.ID, ReasonOf(o.Err)})
		}
	}
	sort.Slice(m, func(i, j int) bool { return m[i].ServerID < m[j].ServerID })
	return m
}

// Fanout calls every peer in parallel. Each call gets its own deadline of
// perServer (also when the others take longer), so one slow server delays the
// answer by at most that. The outcomes are in the order of peers.
func Fanout(ctx context.Context, peers []Peer, perServer time.Duration, call func(ctx context.Context, p Peer) ([]byte, error)) []Outcome {
	out := make([]Outcome, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, perServer)
			defer cancel()
			body, err := call(cctx, p)
			if err == nil && cctx.Err() != nil {
				err = cctx.Err()
			}
			out[i] = Outcome{Peer: p, Body: body, Err: err}
		}()
	}
	wg.Wait()
	return out
}

// ------------------------------------------------------------------- cursors

type cursorDoc struct {
	V    int    `json:"v"`
	Kind string `json:"k"`
	T    int64  `json:"t"`
	S    string `json:"s,omitempty"`
	N    int    `json:"n,omitempty"`
	I    int64  `json:"i,omitempty"`
}

func encodeCursor(d cursorDoc) string {
	b, _ := json.Marshal(d)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ErrCursor is returned for a cursor that this server did not issue.
var ErrCursor = errors.New("the cursor is not valid")

func decodeCursor(s, kind string) (cursorDoc, error) {
	var d cursorDoc
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) > 1024 {
		return d, ErrCursor
	}
	if json.Unmarshal(b, &d) != nil || d.V != 1 || d.Kind != kind || d.T <= 0 {
		return d, ErrCursor
	}
	return d, nil
}

// LogKey is the position of a log entry in the cluster-wide order: newest
// first, then by server id (larger first), then by Rank, the position among the
// entries of that server with the same timestamp.
type LogKey struct {
	Time   time.Time
	Server string
	Rank   int
}

// EncodeLogCursor makes the opaque cursor of a position.
func EncodeLogCursor(k LogKey) string {
	return encodeCursor(cursorDoc{V: 1, Kind: "log", T: k.Time.UnixNano(), S: k.Server, N: k.Rank})
}

// DecodeLogCursor reads a cursor made by EncodeLogCursor.
func DecodeLogCursor(s string) (LogKey, error) {
	d, err := decodeCursor(s, "log")
	if err != nil || len(d.S) > 253 || d.N < 0 {
		return LogKey{}, ErrCursor
	}
	return LogKey{Time: time.Unix(0, d.T).UTC(), Server: d.S, Rank: d.N}, nil
}

// before reports whether a is listed before b.
func (a LogKey) before(b LogKey) bool {
	switch {
	case !a.Time.Equal(b.Time):
		return a.Time.After(b.Time)
	case a.Server != b.Server:
		return a.Server > b.Server
	}
	return a.Rank < b.Rank
}

// RankedLog is a log entry with its rank (see LogKey).
type RankedLog struct {
	Entry api.LogEntry `json:"entry"`
	Rank  int          `json:"rank"`
}

// Key is the entry's position.
func (r RankedLog) Key() LogKey {
	srv := ""
	if r.Entry.ServerID != nil {
		srv = *r.Entry.ServerID
	}
	return LogKey{Time: r.Entry.Timestamp, Server: srv, Rank: r.Rank}
}

// RankLogs sorts the entries of one server newest first and gives each its rank.
func RankLogs(entries []api.LogEntry) []RankedLog {
	es := append([]api.LogEntry(nil), entries...)
	sort.SliceStable(es, func(i, j int) bool { return es[i].Timestamp.After(es[j].Timestamp) })
	out := make([]RankedLog, len(es))
	rank := 0
	for i, e := range es {
		if i > 0 && e.Timestamp.Equal(es[i-1].Timestamp) {
			rank++
		} else {
			rank = 0
		}
		out[i] = RankedLog{Entry: e, Rank: rank}
	}
	return out
}

// AfterLog returns at most limit entries of a ranked list that come after the
// cursor (all of them from the start when after is nil).
func AfterLog(list []RankedLog, after *LogKey, limit int) []RankedLog {
	var out []RankedLog
	for _, r := range list {
		if after != nil && !after.before(r.Key()) {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// MergeLogs merges per-server lists (each in its own order) into one page of at
// most limit entries after the cursor, and the position to continue after (nil
// when nothing is left).
func MergeLogs(lists [][]RankedLog, after *LogKey, limit int) ([]api.LogEntry, *LogKey) {
	if limit <= 0 {
		return []api.LogEntry{}, nil
	}
	var all []RankedLog
	for _, l := range lists {
		for _, r := range l {
			if after != nil && !after.before(r.Key()) {
				continue
			}
			all = append(all, r)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Key().before(all[j].Key()) })
	items := []api.LogEntry{}
	for i, r := range all {
		if i >= limit {
			k := all[limit-1].Key()
			return items, &k
		}
		items = append(items, r.Entry)
	}
	return items, nil
}

// ------------------------------------------------------------------- alerts

// AlertKey is the position of an alert: newest first, the larger id first.
type AlertKey struct {
	Time time.Time
	ID   int64
}

// EncodeAlertCursor makes the opaque cursor of a position.
func EncodeAlertCursor(k AlertKey) string {
	return encodeCursor(cursorDoc{V: 1, Kind: "alert", T: k.Time.UnixNano(), I: k.ID})
}

// DecodeAlertCursor reads a cursor made by EncodeAlertCursor.
func DecodeAlertCursor(s string) (AlertKey, error) {
	d, err := decodeCursor(s, "alert")
	if err != nil || d.I <= 0 {
		return AlertKey{}, ErrCursor
	}
	return AlertKey{Time: time.Unix(0, d.T).UTC(), ID: d.I}, nil
}

// Before reports whether a is listed before b.
func (a AlertKey) Before(b AlertKey) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	return a.ID > b.ID
}

// AlertID is the id of the alert localID of the server with the ordinal: the
// ordinal in the high bits, so that the ids of the alerts of different servers
// never collide and the server that holds an alert is known from its id. Both
// parts are limited so that the id stays below 2^53, the largest integer a JSON
// number holds in a browser.
func AlertID(ordinal int, localID int64) int64 { return int64(ordinal)<<32 | localID&(1<<32-1) }

// SplitAlertID is the inverse of AlertID.
func SplitAlertID(id int64) (ordinal int, localID int64) { return int(id >> 32), id & (1<<32 - 1) }

// MaxAlertOrdinal is the largest ordinal that keeps alert ids below 2^53.
const MaxAlertOrdinal = 1<<21 - 1

// MergeAlerts merges per-server lists into one page of at most limit alerts
// after the cursor.
func MergeAlerts(lists [][]api.Alert, after *AlertKey, limit int) ([]api.Alert, *AlertKey) {
	if limit <= 0 {
		return []api.Alert{}, nil
	}
	var all []api.Alert
	for _, l := range lists {
		for _, a := range l {
			if after != nil && !after.Before(AlertKey{a.RaisedAt, a.ID}) {
				continue
			}
			all = append(all, a)
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		return AlertKey{all[i].RaisedAt, all[i].ID}.Before(AlertKey{all[j].RaisedAt, all[j].ID})
	})
	items := []api.Alert{}
	for i, a := range all {
		if i >= limit {
			k := AlertKey{all[limit-1].RaisedAt, all[limit-1].ID}
			return items, &k
		}
		items = append(items, a)
	}
	return items, nil
}
