package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/store"
)

type ProxyStats struct {
	ProxyID  string `json:"proxy_id"`
	Provider string `json:"provider"`
	store.Stats
}

type ProviderCounts struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
}

type PublicProxy struct {
	ProxyID  string `json:"proxy_id"`
	Provider string `json:"provider"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
}

func List(ctx context.Context, db store.DB) ([]PublicProxy, error) {
	all, err := db.All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]PublicProxy, 0)
	for _, proxies := range all {
		for _, p := range proxies {
			result = append(result, PublicProxy{p.ProxyID, p.Provider, p.Host, p.Port})
		}
	}
	slices.SortFunc(result, func(a, b PublicProxy) int { return strings.Compare(a.ProxyID, b.ProxyID) })
	return result, nil
}

func Stats(ctx context.Context, db store.DB) ([]ProxyStats, error) {
	all, err := db.All(ctx)
	if err != nil {
		return nil, err
	}
	count := 0
	for _, providerProxies := range all {
		count += len(providerProxies)
	}
	proxies := make([]store.Proxy, 0, count)
	for _, providerProxies := range all {
		proxies = append(proxies, providerProxies...)
	}
	ids := make([]string, len(proxies))
	for i, p := range proxies {
		ids[i] = p.ProxyID
	}
	stats, err := db.GetStatsMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make([]ProxyStats, len(proxies))
	for i, p := range proxies {
		result[i] = ProxyStats{p.ProxyID, p.Provider, stats[i]}
	}
	slices.SortFunc(result, func(a, b ProxyStats) int { return strings.Compare(a.ProxyID, b.ProxyID) })
	return result, nil
}

func Handler(db store.DB, routes config.Routes) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	failure := func(w http.ResponseWriter) {
		reply(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
	}
	index := struct {
		Service   string            `json:"service"`
		Endpoints map[string]string `json:"endpoints"`
	}{
		Service: "proxy-manager",
		Endpoints: map[string]string{
			"health": "/health", "stats": "/stats", "stats_detail": "/stats/{proxy_id}",
			"providers": "/providers", "routes": "/routes", "metrics": "/metrics",
		},
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			reply(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		reply(w, http.StatusOK, index)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if db.Ping(r.Context()) != nil {
			failure(w)
			return
		}
		reply(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		rows, err := Stats(r.Context(), db)
		if err != nil {
			failure(w)
			return
		}
		reply(w, http.StatusOK, rows)
	})
	mux.HandleFunc("GET /stats/{proxy_id}", func(w http.ResponseWriter, r *http.Request) {
		stats, err := db.GetStats(r.Context(), r.PathValue("proxy_id"))
		if err != nil {
			failure(w)
			return
		}
		reply(w, http.StatusOK, stats)
	})
	mux.HandleFunc("GET /providers", func(w http.ResponseWriter, r *http.Request) {
		all, err := db.All(r.Context())
		if err != nil {
			failure(w)
			return
		}
		result := make(map[string]ProviderCounts, len(all))
		for name, proxies := range all {
			healthy, err := db.HealthyMany(r.Context(), proxies)
			if err != nil {
				failure(w)
				return
			}
			result[name] = ProviderCounts{len(proxies), len(healthy), len(proxies) - len(healthy)}
		}
		reply(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		body, err := metrics(r.Context(), db)
		if err != nil {
			failure(w)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET /routes", func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, routes) })
	return mux
}
