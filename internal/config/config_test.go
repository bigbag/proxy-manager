package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadListenerHosts(t *testing.T) {
	for _, name := range []string{"PROXY_HOST", "API_HOST"} {
		value, ok := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Proxy.Host != "127.0.0.1" || got.API.Host != "127.0.0.1" {
		t.Fatalf("listener hosts = proxy %q, API %q", got.Proxy.Host, got.API.Host)
	}

	t.Setenv("PROXY_HOST", "0.0.0.0")
	t.Setenv("API_HOST", "192.0.2.1")
	got, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Proxy.Host != "0.0.0.0" || got.API.Host != "192.0.2.1" {
		t.Fatalf("configured listener hosts = proxy %q, API %q", got.Proxy.Host, got.API.Host)
	}
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	t.Setenv("STORE", "redis")
	t.Setenv("PROXY_PORT", "8080")
	t.Setenv("PROXY_MAX_CONNECTIONS", "1024")
	t.Setenv("PROXY_AFFINITY_TTL", "1800")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Store != "redis" || got.Proxy.Port != 8080 || got.Proxy.MaxConnections != 1024 || got.Proxy.BufferSize != 16384 || got.Proxy.AffinityTTL != 1800 {
		t.Fatalf("defaults: %+v", got)
	}
	for _, tc := range []struct{ name, value string }{
		{"PROXY_PORT", "65536"},
		{"PROXY_MAX_CONNECTIONS", "0"},
		{"PROXY_CONNECT_TIMEOUT", "0"},
		{"PROXY_AFFINITY_TTL", "-1"},
		{"HEALTH_FAILURE_THRESHOLD", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("accepted %s=%q", tc.name, tc.value)
			}
		})
	}
}

func TestLoadProvidersFromFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "providers.json")
	t.Setenv("PROVIDERS_CONFIG_PATH", file)
	if err := os.WriteFile(file, []byte(`{"rayobyte":{"api_key":"secret","api_login":"login","refresh_interval_minutes":15},"webshare":{"api_key":"other"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := Load()
	if err != nil || len(settings.Providers) != 2 || settings.Providers["rayobyte"].APIKey != "secret" || settings.Providers["rayobyte"].RefreshIntervalMinutes != 15 {
		t.Fatalf("provider settings = %+v, %v", settings.Providers, err)
	}
	for _, invalid := range []string{`{"webshare":{"api_keey":"typo"}}`, `{"webshare":{"refresh_interval_minutes":-1}}`, `{"new_provider":{}}`, `null`} {
		if err := os.WriteFile(file, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("accepted provider configuration %q", invalid)
		}
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("accepted missing explicit provider file")
	}
}

func TestLoadRoutesDefaultAndInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	routes, err := LoadRoutes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes.Routes) != 1 || routes.Routes[0].Pattern != "*" || routes.Routes[0].Strategy != PriorityFallback || len(routes.Routes[0].Providers) != 3 {
		t.Fatalf("default routes = %+v", routes)
	}
	if err := os.WriteFile(path, []byte(`{"routes":[{"pattern":"[","providers":["webshare"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRoutes(path); err == nil {
		t.Fatal("accepted invalid glob")
	}
	if err := os.WriteFile(path, []byte(`{"routes":[{"pattern":"*","providers":["webshare"],"strategy":"random"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRoutes(path); err == nil {
		t.Fatal("accepted unknown strategy")
	}
}
