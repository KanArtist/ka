package engine

import (
	"sort"
	"sync"
	"time"
)

// OpStats counts operations by kind and outcome, for /metrics.
type OpStats struct {
	mu      sync.Mutex
	counts  map[[2]string]uint64 // {op, result} -> n
	seconds map[string]float64   // op -> total seconds
}

// OpCount is one row of operation statistics.
type OpCount struct {
	Op, Result string
	N          uint64
}

func (m *OpStats) record(op string, err error, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts, m.seconds = map[[2]string]uint64{}, map[string]float64{}
	}
	res := "ok"
	if err != nil {
		res = AsError(err).Code
	}
	m.counts[[2]string{op, res}]++
	m.seconds[op] += d.Seconds()
}

// Snapshot returns the counts, sorted, and the total seconds per op.
func (m *OpStats) Snapshot() ([]OpCount, map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]OpCount, 0, len(m.counts))
	for k, n := range m.counts {
		out = append(out, OpCount{k[0], k[1], n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Op < out[j].Op || out[i].Op == out[j].Op && out[i].Result < out[j].Result })
	secs := make(map[string]float64, len(m.seconds))
	for k, v := range m.seconds {
		secs[k] = v
	}
	return out, secs
}

// Ops returns the service's operation statistics.
func (s *Service) Ops() *OpStats { return &s.ops }

// TicketCounts counts tickets by project and status category.
func (s *Service) TicketCounts() map[[2]string]int {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	out := map[[2]string]int{}
	for _, r := range s.ix.tickets {
		cat := "unknown"
		if st, ok := r.Project.Cfg.StatusByName(r.T.Status); ok {
			cat = st.Category
		}
		out[[2]string{r.Project.Key, cat}]++
	}
	return out
}
