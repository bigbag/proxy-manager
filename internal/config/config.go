package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
)

type Strategy string

const (
	RoundRobin       Strategy = "round_robin"
	PriorityFallback Strategy = "priority_fallback"
)

type Route struct {
	Pattern   string   `json:"pattern"`
	Providers []string `json:"providers"`
	Strategy  Strategy `json:"strategy"`
}

type Routes struct {
	Routes []Route `json:"routes"`
}

type ProxySettings struct {
	Host           string
	Port           int
	ConnectTimeout int
	BufferSize     int
	AffinityTTL    int
	MaxConnections int
}

type APISettings struct {
	Host string
	Port int
}
type HealthSettings struct {
	FailureThreshold int
	CooldownSeconds  int
}
type ProviderSettings struct {
	APIKey                 string `json:"api_key"`
	APILogin               string `json:"api_login"`
	BaseURL                string `json:"base_url"`
	Address                string `json:"address"`
	InitialPort            int    `json:"initial_port"`
	Slots                  int    `json:"slots"`
	RefreshIntervalMinutes int    `json:"refresh_interval_minutes"`
}

type Settings struct {
	LogLevel, Store, RedisURL, SQLitePath, RoutesConfigPath string
	Proxy                                                   ProxySettings
	API                                                     APISettings
	Health                                                  HealthSettings
	Providers                                               map[string]ProviderSettings
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

func Load() (Settings, error) {
	var s Settings
	var err error
	s.LogLevel = env("LOG_LEVEL", "INFO")
	s.Store = strings.ToLower(env("STORE", "redis"))
	if s.Store != "redis" && s.Store != "sqlite" {
		return s, fmt.Errorf("STORE: invalid value %q", s.Store)
	}
	s.RedisURL = env("REDIS_URL", "redis://127.0.0.1:6379/0")
	s.SQLitePath = env("SQLITE_PATH", "proxy.db")
	s.RoutesConfigPath = env("ROUTES_CONFIG_PATH", "routes.json")
	s.Proxy.Host = env("PROXY_HOST", "127.0.0.1")
	if s.Proxy.Port, err = envInt("PROXY_PORT", 8080); err != nil {
		return s, err
	}
	if s.Proxy.ConnectTimeout, err = envInt("PROXY_CONNECT_TIMEOUT", 30); err != nil {
		return s, err
	}
	if s.Proxy.BufferSize, err = envInt("PROXY_BUFFER_SIZE", 16384); err != nil {
		return s, err
	}
	if s.Proxy.AffinityTTL, err = envInt("PROXY_AFFINITY_TTL", 1800); err != nil {
		return s, err
	}
	if s.Proxy.MaxConnections, err = envInt("PROXY_MAX_CONNECTIONS", 1024); err != nil {
		return s, err
	}
	s.API.Host = env("API_HOST", "127.0.0.1")
	if s.API.Port, err = envInt("API_PORT", 8081); err != nil {
		return s, err
	}
	if s.Health.FailureThreshold, err = envInt("HEALTH_FAILURE_THRESHOLD", 3); err != nil {
		return s, err
	}
	if s.Health.CooldownSeconds, err = envInt("HEALTH_COOLDOWN_SECONDS", 300); err != nil {
		return s, err
	}
	for _, value := range []struct {
		name string
		n    int
	}{
		{"PROXY_PORT", s.Proxy.Port}, {"API_PORT", s.API.Port},
	} {
		if value.n < 1 || value.n > 65535 {
			return s, fmt.Errorf("%s: invalid port %d", value.name, value.n)
		}
	}
	for _, value := range []struct {
		name string
		n    int
	}{
		{"PROXY_CONNECT_TIMEOUT", s.Proxy.ConnectTimeout}, {"PROXY_BUFFER_SIZE", s.Proxy.BufferSize},
		{"PROXY_MAX_CONNECTIONS", s.Proxy.MaxConnections}, {"HEALTH_FAILURE_THRESHOLD", s.Health.FailureThreshold},
		{"HEALTH_COOLDOWN_SECONDS", s.Health.CooldownSeconds},
	} {
		if value.n <= 0 {
			return s, fmt.Errorf("%s: value must be positive", value.name)
		}
	}
	if s.Proxy.AffinityTTL < 0 {
		return s, fmt.Errorf("PROXY_AFFINITY_TTL: value must not be negative")
	}
	file, explicit := os.LookupEnv("PROVIDERS_CONFIG_PATH")
	if !explicit {
		file = "providers.json"
	}
	s.Providers, err = loadProviders(file, !explicit)
	if err != nil {
		return s, err
	}
	return s, nil
}

func loadProviders(file string, optional bool) (map[string]ProviderSettings, error) {
	// #nosec G304 -- The operator sets the provider file path.
	f, err := os.Open(file)
	if os.IsNotExist(err) && optional {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("PROVIDERS_CONFIG_PATH: %w", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	var providers map[string]ProviderSettings
	if err := decoder.Decode(&providers); err != nil {
		return nil, fmt.Errorf("PROVIDERS_CONFIG_PATH: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("PROVIDERS_CONFIG_PATH: trailing data")
	}
	if providers == nil {
		return nil, fmt.Errorf("PROVIDERS_CONFIG_PATH: expected provider object")
	}
	for name, settings := range providers {
		if name == "" || strings.ContainsAny(name, "_:") || settings.RefreshIntervalMinutes < 0 || settings.InitialPort < 0 || settings.InitialPort > 65535 || settings.Slots < 0 {
			return nil, fmt.Errorf("PROVIDERS_CONFIG_PATH: invalid settings for provider %q", name)
		}
	}
	return providers, nil
}

func LoadRoutes(file string) (Routes, error) {
	// #nosec G304 -- The operator sets the route file path.
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return Routes{Routes: []Route{{Pattern: "*", Providers: []string{"webshare", "proxyrack", "rayobyte"}, Strategy: PriorityFallback}}}, nil
	}
	if err != nil {
		return Routes{}, err
	}
	var routes Routes
	if err := json.Unmarshal(raw, &routes); err != nil {
		return Routes{}, err
	}
	for i := range routes.Routes {
		r := &routes.Routes[i]
		if _, err := path.Match(r.Pattern, ""); err != nil {
			return Routes{}, fmt.Errorf("route %d: %w", i, err)
		}
		if r.Pattern == "" {
			return Routes{}, fmt.Errorf("route %d: empty pattern", i)
		}
		if r.Strategy == "" {
			r.Strategy = RoundRobin
		}
		if r.Strategy != RoundRobin && r.Strategy != PriorityFallback {
			return Routes{}, fmt.Errorf("route %d: invalid strategy %q", i, r.Strategy)
		}
	}
	return routes, nil
}
