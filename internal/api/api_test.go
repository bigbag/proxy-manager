package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/store"
)

func TestAPIReportsSafeStatsCountsAndRoutes(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite", "", filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := store.NewProxy("webshare", "example.com", 8080, "secret-user", "secret-pass")
	if err := db.Replace(ctx, p.Provider, []store.Proxy{p}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRequest(ctx, p.ProxyID, p.Provider, true, 10*time.Millisecond, 30); err != nil {
		t.Fatal(err)
	}
	routes := config.Routes{Routes: []config.Route{{Pattern: "*", Providers: []string{"webshare"}, Strategy: config.RoundRobin}}}
	handler := Handler(db, routes)
	for _, path := range []string{"/", "/health", "/stats", "/stats/" + p.ProxyID, "/stats/unknown", "/providers", "/routes"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 {
			t.Fatalf("%s status = %d", path, response.Code)
		}
		if !json.Valid(response.Body.Bytes()) {
			t.Fatalf("%s invalid JSON: %s", path, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret-") {
			t.Fatalf("%s exposed credentials", path)
		}
	}
	indexResponse := httptest.NewRecorder()
	handler.ServeHTTP(indexResponse, httptest.NewRequest(http.MethodGet, "/", nil))
	var index struct {
		Endpoints map[string]string `json:"endpoints"`
	}
	if err := json.Unmarshal(indexResponse.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"health": "/health", "stats": "/stats", "stats_detail": "/stats/{proxy_id}",
		"providers": "/providers", "routes": "/routes", "metrics": "/metrics",
	}
	if !maps.Equal(index.Endpoints, expected) {
		t.Fatalf("API endpoints = %v, want %v", index.Endpoints, expected)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/stats/unknown", nil))
	var unknown struct {
		SuccessCount int64 `json:"success_count"`
		FailureCount int64 `json:"failure_count"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &unknown); err != nil || unknown.SuccessCount != 0 || unknown.FailureCount != 0 {
		t.Fatalf("missing proxy stats: %s, %v", response.Body.String(), err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/providers", nil))
	if !strings.Contains(response.Body.String(), `"healthy":1`) {
		t.Fatalf("provider counts: %s", response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/stats", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d", response.Code)
	}
}

func TestAPIHealthFailsWhenStoreUnavailable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite", "", filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler(db, config.Routes{})
	db.Close()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/health", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("health = %d", response.Code)
	}
}

func TestMetricsExportsSafePrometheusSamples(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite", "", filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	proxy := store.NewProxy("web\"\\\nshare", "127.0.0.1", 8080, "api-secret", "upstream-secret")
	if err := db.Replace(ctx, proxy.Provider, []store.Proxy{proxy}); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		success bool
		latency time.Duration
		bytes   int64
	}{{true, 10 * time.Millisecond, 30}, {false, 20 * time.Millisecond, 20}} {
		if err := db.RecordRequest(ctx, proxy.ProxyID, proxy.Provider, sample.success, sample.latency, sample.bytes); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.RecordFailure(ctx, proxy.ProxyID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	handler := Handler(db, config.Routes{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics response = %d %q: %s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	for _, want := range []string{
		"# TYPE proxy_manager_proxies gauge\n",
		`proxy_manager_proxies{provider="web\"\\\nshare",health="healthy"} 0` + "\n",
		`proxy_manager_proxies{provider="web\"\\\nshare",health="unhealthy"} 1` + "\n",
		"# TYPE proxy_manager_requests_total counter\n",
		`proxy_manager_requests_total{provider="web\"\\\nshare",result="success"} 1` + "\n",
		`proxy_manager_requests_total{provider="web\"\\\nshare",result="failure"} 1` + "\n",
		`proxy_manager_bytes_transferred_total{provider="web\"\\\nshare"} 50` + "\n",
		"# TYPE proxy_manager_connect_duration_seconds summary\n",
		`proxy_manager_connect_duration_seconds_sum{provider="web\"\\\nshare"} 0.03` + "\n",
		`proxy_manager_connect_duration_seconds_count{provider="web\"\\\nshare"} 2` + "\n",
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("metrics missing %q: %s", want, response.Body.String())
		}
	}
	if strings.Contains(response.Body.String(), "proxy_id=") || strings.Contains(response.Body.String(), "api-secret") || strings.Contains(response.Body.String(), "upstream-secret") {
		t.Fatal("metrics exposed proxy details or credentials")
	}
	if err := db.Replace(ctx, proxy.Provider, nil); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`proxy_manager_proxies{provider="web\"\\\nshare",health="healthy"} 0` + "\n",
		`proxy_manager_proxies{provider="web\"\\\nshare",health="unhealthy"} 0` + "\n",
		`proxy_manager_requests_total{provider="web\"\\\nshare",result="success"} 1` + "\n",
		`proxy_manager_requests_total{provider="web\"\\\nshare",result="failure"} 1` + "\n",
		`proxy_manager_bytes_transferred_total{provider="web\"\\\nshare"} 50` + "\n",
		`proxy_manager_connect_duration_seconds_sum{provider="web\"\\\nshare"} 0.03` + "\n",
		`proxy_manager_connect_duration_seconds_count{provider="web\"\\\nshare"} 2` + "\n",
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("metrics changed after refresh: missing %q in %s", want, response.Body.String())
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "proxy_manager_") {
		t.Fatalf("failed metrics response = %d: %s", response.Code, response.Body.String())
	}
}

type statsCallCounter struct {
	store.DB
	single int
	batch  int
}

func (s *statsCallCounter) GetStats(ctx context.Context, id string) (store.Stats, error) {
	s.single++
	return s.DB.GetStats(ctx, id)
}

func (s *statsCallCounter) GetStatsMany(ctx context.Context, ids []string) ([]store.Stats, error) {
	s.batch++
	return s.DB.GetStatsMany(ctx, ids)
}

func TestAPIStatsBatchesProvidersAndSortsByProxyID(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite", "", filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	proxies := []store.Proxy{
		store.NewProxy("zeta", "127.0.0.1", 1, "", ""),
		store.NewProxy("alpha", "127.0.0.2", 2, "", ""),
	}
	for _, p := range proxies {
		if err := db.Replace(ctx, p.Provider, []store.Proxy{p}); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordRequest(ctx, p.ProxyID, p.Provider, true, time.Millisecond, int64(p.Port)); err != nil {
			t.Fatal(err)
		}
	}
	counted := &statsCallCounter{DB: db}
	got, err := Stats(ctx, counted)
	if err != nil {
		t.Fatal(err)
	}
	if counted.batch != 1 || counted.single != 0 {
		t.Fatalf("stats calls = batch %d, single %d", counted.batch, counted.single)
	}
	if len(got) != 2 || got[0].ProxyID != proxies[1].ProxyID || got[1].ProxyID != proxies[0].ProxyID {
		t.Fatalf("stats order = %+v", got)
	}
	if got[0].SuccessCount != 1 || got[0].BytesTransferred != 2 || got[1].SuccessCount != 1 || got[1].BytesTransferred != 1 {
		t.Fatalf("stats values = %+v", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Stats(ctx, counted); err == nil {
		t.Fatal("stats succeeded after store closed")
	}
}
