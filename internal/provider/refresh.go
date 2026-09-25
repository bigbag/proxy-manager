package provider

import (
	"context"
	"log/slog"
	"time"

	"github.com/bigbag/proxy-manager/internal/store"
)

type Fetcher interface {
	Name() string
	Interval() time.Duration
	Fetch(context.Context) ([]store.Proxy, error)
}

type Refresher struct {
	Store    store.DB
	Fetchers []Fetcher
	Now      func() time.Time
	last     map[string]time.Time
}

func (r *Refresher) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Refresher) Needs(f Fetcher) bool {
	return r.now().Sub(r.last[f.Name()]) >= f.Interval()
}

func (r *Refresher) RefreshAll(ctx context.Context) map[string]int {
	counts := make(map[string]int, len(r.Fetchers))
	if len(r.Fetchers) == 0 {
		slog.Warn("no providers enabled", "component", "provider")
	}
	for _, f := range r.Fetchers {
		if ctx.Err() != nil {
			break
		}
		counts[f.Name()] = r.refresh(ctx, f)
	}
	return counts
}

func (r *Refresher) refresh(ctx context.Context, f Fetcher) int {
	slog.Info("refreshing provider", "component", "provider", "provider", f.Name())
	list, err := f.Fetch(ctx)
	if err == nil {
		err = r.Store.Replace(ctx, f.Name(), list)
	}
	if err != nil {
		slog.Error("provider refresh failed", "component", "provider", "provider", f.Name(), "error", err)
		return 0
	}
	if r.last == nil {
		r.last = make(map[string]time.Time)
	}
	r.last[f.Name()] = r.now()
	slog.Info("provider refreshed", "component", "provider", "provider", f.Name(), "count", len(list))
	return len(list)
}

func (r *Refresher) Run(ctx context.Context) error {
	r.RefreshAll(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for _, f := range r.Fetchers {
				if r.Needs(f) {
					r.refresh(ctx, f)
				}
			}
		}
	}
}
