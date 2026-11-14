// SPDX-License-Identifier: PostgreSQL

package monitor

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Series identifies one time series.
type Series struct {
	Metric   string
	ServerID string // empty for database-scoped and cluster-scoped metrics
	Database string // empty for server-scoped metrics
}

// Point is one sample; Value nil means "not available".
type Point struct {
	T time.Time
	V *float64
}

// Ring keeps the most recent samples of every series in memory. The number of
// series and the number of points per series are bounded, so memory does not
// grow with time or with the number of databases.
type Ring struct {
	mu        sync.Mutex
	perSeries int
	maxSeries int
	data      map[Series][]Point
}

// NewRing returns a ring buffer holding at most perSeries points of at most
// maxSeries series (new series beyond the limit are dropped).
func NewRing(perSeries, maxSeries int) *Ring {
	return &Ring{perSeries: perSeries, maxSeries: maxSeries, data: map[Series][]Point{}}
}

// Add appends a sample.
func (r *Ring) Add(s Series, t time.Time, v *float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pts, ok := r.data[s]
	if !ok && len(r.data) >= r.maxSeries {
		return
	}
	pts = append(pts, Point{t, v})
	if len(pts) > r.perSeries {
		pts = append(pts[:0:0], pts[len(pts)-r.perSeries:]...)
	}
	r.data[s] = pts
}

// Result is one resampled series.
type Result struct {
	Series Series
	Points []Point
}

// MaxPoints is the contract's limit of points per series.
const MaxPoints = 5000

// ErrTooManyPoints is returned when from, to and step would give more points.
var ErrTooManyPoints = fmt.Errorf("a query returns at most %d points per series", MaxPoints)

// Query resamples the series of a metric into buckets of step seconds between
// from and to (the average of the samples in a bucket; a bucket without
// samples is null). server and database restrict the series when non-empty;
// the aggregate over servers/databases (average for percentages, sum
// otherwise) is returned when groupBy is empty and no restriction names one.
func (r *Ring) Query(metric string, from, to time.Time, step int, server, database, groupBy string, sum bool) ([]Result, error) {
	n := int(to.Sub(from).Seconds())/step + 1
	if n > MaxPoints {
		return nil, ErrTooManyPoints
	}
	if n < 1 {
		n = 1
	}
	r.mu.Lock()
	var matching []Series
	for s := range r.data {
		if s.Metric != metric || (server != "" && s.ServerID != server) || (database != "" && s.Database != database) {
			continue
		}
		matching = append(matching, s)
	}
	sort.Slice(matching, func(i, j int) bool {
		if matching[i].ServerID != matching[j].ServerID {
			return matching[i].ServerID < matching[j].ServerID
		}
		return matching[i].Database < matching[j].Database
	})
	resampled := make(map[Series][]Point, len(matching))
	for _, s := range matching {
		resampled[s] = resample(r.data[s], from, step, n)
	}
	r.mu.Unlock()
	if groupBy == "" && server == "" && database == "" {
		return []Result{{Series{Metric: metric}, combine(resampled, matching, from, step, n, sum)}}, nil
	}
	out := make([]Result, 0, len(matching))
	for _, s := range matching {
		out = append(out, Result{s, resampled[s]})
	}
	return out, nil
}

func resample(pts []Point, from time.Time, step, n int) []Point {
	sums, cnt := make([]float64, n), make([]int, n)
	for _, p := range pts {
		if p.V == nil {
			continue
		}
		i := int(p.T.Sub(from).Seconds()) / step
		if p.T.Before(from) || i < 0 || i >= n {
			continue
		}
		sums[i] += *p.V
		cnt[i]++
	}
	out := make([]Point, n)
	for i := range out {
		out[i].T = from.Add(time.Duration(i*step) * time.Second)
		if cnt[i] > 0 {
			v := sums[i] / float64(cnt[i])
			out[i].V = &v
		}
	}
	return out
}

func combine(by map[Series][]Point, order []Series, from time.Time, step, n int, sum bool) []Point {
	out := make([]Point, n)
	for i := range out {
		out[i].T = from.Add(time.Duration(i*step) * time.Second)
		var total float64
		var c int
		for _, s := range order {
			if v := by[s][i].V; v != nil {
				total += *v
				c++
			}
		}
		if c > 0 {
			if !sum {
				total /= float64(c)
			}
			out[i].V = &total
		}
	}
	return out
}

// Latest returns the newest non-null sample of every series.
func (r *Ring) Latest() map[Series]Point {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[Series]Point{}
	for s, pts := range r.data {
		for i := len(pts) - 1; i >= 0; i-- {
			if pts[i].V != nil {
				out[s] = pts[i]
				break
			}
		}
	}
	return out
}

// WritePrometheus writes the latest values in the Prometheus text format.
func (r *Ring) WritePrometheus(w io.Writer) {
	latest := r.Latest()
	keys := make([]Series, 0, len(latest))
	for s := range latest {
		keys = append(keys, s)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Metric != keys[j].Metric {
			return keys[i].Metric < keys[j].Metric
		}
		if keys[i].ServerID != keys[j].ServerID {
			return keys[i].ServerID < keys[j].ServerID
		}
		return keys[i].Database < keys[j].Database
	})
	last := ""
	for _, s := range keys {
		name := "kingres_" + snake(s.Metric)
		if name != last {
			fmt.Fprintf(w, "# TYPE %s gauge\n", name)
			last = name
		}
		var labels []string
		if s.ServerID != "" {
			labels = append(labels, `server="`+escapeLabel(s.ServerID)+`"`)
		}
		if s.Database != "" {
			labels = append(labels, `database="`+escapeLabel(s.Database)+`"`)
		}
		l := ""
		if len(labels) > 0 {
			l = "{" + strings.Join(labels, ",") + "}"
		}
		fmt.Fprintf(w, "%s%s %g\n", name, l, *latest[s].V)
	}
}

func snake(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			sb.WriteByte('_')
			r += 'a' - 'A'
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}
