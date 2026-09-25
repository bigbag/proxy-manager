package pool

import (
	"context"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/store"
)

func Select(ctx context.Context, db store.DB, providers []string, strategy config.Strategy, key string, ttl time.Duration) (*store.Proxy, error) {
	if len(providers) == 0 {
		return nil, nil
	}
	if key != "" && ttl > 0 {
		id, err := db.GetAffinity(ctx, key)
		if err != nil {
			return nil, err
		}
		if id != "" {
			bound, err := db.ByID(ctx, id)
			if err != nil {
				return nil, err
			}
			if bound != nil && slices.Contains(providers, bound.Provider) {
				healthy, err := db.Healthy(ctx, bound.ProxyID)
				if err != nil {
					return nil, err
				}
				if healthy {
					return bound, nil
				}
			}
			return Fallback(ctx, db, providers, "")
		}
	}
	switch strategy {
	case config.RoundRobin:
		healthy, err := healthyFrom(ctx, db, providers)
		if err != nil {
			return nil, err
		}
		if len(healthy) == 0 {
			return randomFrom(ctx, db, providers)
		}
		sort.Slice(healthy, func(i, j int) bool { return healthy[i].ProxyID < healthy[j].ProxyID })
		names := slices.Clone(providers)
		sort.Strings(names)
		return at(ctx, db, healthy, strings.Join(names, ","))
	case config.PriorityFallback:
		for _, provider := range providers {
			list, err := db.List(ctx, provider)
			if err != nil {
				return nil, err
			}
			healthy, err := db.HealthyMany(ctx, list)
			if err != nil {
				return nil, err
			}
			if len(healthy) == 0 {
				continue
			}
			sort.Slice(healthy, func(i, j int) bool { return healthy[i].ProxyID < healthy[j].ProxyID })
			return at(ctx, db, healthy, provider)
		}
		return randomFrom(ctx, db, providers)
	default:
		return nil, nil
	}
}

func at(ctx context.Context, db store.DB, list []store.Proxy, key string) (*store.Proxy, error) {
	n, err := db.NextIndex(ctx, key)
	if err != nil {
		return nil, err
	}
	selected := list[int((n-1)%int64(len(list)))]
	return &selected, nil
}

func healthyFrom(ctx context.Context, db store.DB, providers []string) ([]store.Proxy, error) {
	var result []store.Proxy
	for _, name := range providers {
		list, err := db.List(ctx, name)
		if err != nil {
			return nil, err
		}
		healthy, err := db.HealthyMany(ctx, list)
		if err != nil {
			return nil, err
		}
		result = append(result, healthy...)
	}
	return result, nil
}

func randomFrom(ctx context.Context, db store.DB, providers []string) (*store.Proxy, error) {
	var list []store.Proxy
	for _, name := range providers {
		proxies, err := db.List(ctx, name)
		if err != nil {
			return nil, err
		}
		list = append(list, proxies...)
	}
	if len(list) == 0 {
		return nil, nil
	}
	// #nosec G404 -- Proxy selection does not need secret randomness.
	selected := list[rand.IntN(len(list))]
	return &selected, nil
}

func Fallback(ctx context.Context, db store.DB, providers []string, failedID string) (*store.Proxy, error) {
	var firstUnhealthy *store.Proxy
	for _, name := range providers {
		list, err := db.List(ctx, name)
		if err != nil {
			return nil, err
		}
		sort.Slice(list, func(i, j int) bool { return list[i].ProxyID < list[j].ProxyID })
		for _, p := range list {
			if p.ProxyID != failedID && firstUnhealthy == nil {
				candidate := p
				firstUnhealthy = &candidate
			}
		}
		healthy, err := db.HealthyMany(ctx, list)
		if err != nil {
			return nil, err
		}
		for _, p := range healthy {
			if p.ProxyID != failedID {
				candidate := p
				return &candidate, nil
			}
		}
	}
	return firstUnhealthy, nil
}
