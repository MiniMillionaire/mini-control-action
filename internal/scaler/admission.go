package scaler

import (
	"context"
	"sync"

	"github.com/Lakr233/mini-control-action/internal/config"
	"github.com/Lakr233/mini-control-action/internal/state"
)

// admission reserves all three limits atomically. Waiting requests hold no
// permits; ready repositories alternate, with FIFO within each repository.
type admission struct {
	mu       sync.Mutex
	cfg      *config.Config
	held     map[state.Key]state.Record
	waiting  []*reservation
	lastRepo string
}

type reservation struct {
	record state.Record
	ready  chan struct{}
}

func newAdmission(cfg *config.Config) *admission {
	return &admission{cfg: cfg, held: map[state.Key]state.Record{}}
}

func (a *admission) seed(records []state.Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rec := range records {
		if !rec.State.Terminal() && (rec.VMID != "" || rec.State == state.StateCreating) {
			a.held[rec.Key()] = rec
		}
	}
}

func (a *admission) acquire(ctx context.Context, rec state.Record) error {
	a.mu.Lock()
	if _, ok := a.held[rec.Key()]; ok {
		a.mu.Unlock()
		return nil
	}
	r := &reservation{record: rec, ready: make(chan struct{})}
	a.waiting = append(a.waiting, r)
	a.dispatch()
	a.mu.Unlock()
	select {
	case <-r.ready:
		return nil
	case <-ctx.Done():
		a.mu.Lock()
		for i, candidate := range a.waiting {
			if candidate == r {
				a.waiting = append(a.waiting[:i], a.waiting[i+1:]...)
				break
			}
		}
		delete(a.held, rec.Key())
		a.dispatch()
		a.mu.Unlock()
		return ctx.Err()
	}
}

func (a *admission) release(key state.Key) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.held, key)
	a.dispatch()
}

func (a *admission) reason(rec state.Record) string {
	if len(a.held) >= a.cfg.Limits.MaxConcurrentVMs {
		return "global_limit"
	}
	if rec.Plan == nil {
		return ""
	}
	_, limit, paused, err := a.cfg.Resolve(rec.Plan.Account, rec.Repo)
	if err != nil {
		return "configuration_missing"
	}
	pool, ok := a.cfg.Pools[rec.Plan.Pool]
	if !ok {
		return "configuration_missing"
	}
	if paused || pool.Paused {
		return "paused"
	}
	poolCount, repoCount := 0, 0
	for _, held := range a.held {
		if held.Namespace == rec.Namespace {
			repoCount++
		}
		if held.Plan != nil && held.Plan.Pool == rec.Plan.Pool {
			poolCount++
		}
	}
	if repoCount >= limit {
		return "repository_limit"
	}
	if poolCount >= pool.MaxConcurrentVMs {
		return "pool_limit"
	}
	return ""
}

func (a *admission) dispatch() {
	for {
		chosen := -1
		for i, r := range a.waiting {
			if a.reason(r.record) != "" {
				continue
			}
			if chosen < 0 {
				chosen = i
			}
			if r.record.Namespace != a.lastRepo {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			return
		}
		r := a.waiting[chosen]
		a.waiting = append(a.waiting[:chosen], a.waiting[chosen+1:]...)
		a.held[r.record.Key()] = r.record
		a.lastRepo = r.record.Namespace
		close(r.ready)
	}
}

func (a *admission) status() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for _, r := range a.waiting {
		out[r.record.Namespace] = a.reason(r.record)
	}
	return out
}
