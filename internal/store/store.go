package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Proxy struct {
	ProxyID  string `json:"proxy_id"`
	Provider string `json:"provider"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
}

func NewProxy(provider, host string, port int, user, password string) Proxy {
	return Proxy{ProxyID: provider + "_" + host + "_" + strconv.Itoa(port), Provider: provider, Host: host, Port: port, User: user, Password: password}
}

func (p Proxy) HasAuth() bool { return p.User != "" && p.Password != "" }

type Stats struct {
	SuccessCount     int64   `json:"success_count"`
	FailureCount     int64   `json:"failure_count"`
	BytesTransferred int64   `json:"bytes_transferred"`
	LatencyCount     int     `json:"latency_count"`
	LatencyAvgMs     float64 `json:"latency_avg_ms"`
	LatencyP50Ms     float64 `json:"latency_p50_ms"`
	LatencyP95Ms     float64 `json:"latency_p95_ms"`
}

type ProviderStats struct {
	SuccessCount     int64
	FailureCount     int64
	BytesTransferred int64
	LatencyCount     int64
	LatencyMicros    int64
}

type DB interface {
	Close() error
	Ping(context.Context) error
	Replace(context.Context, string, []Proxy) error
	List(context.Context, string) ([]Proxy, error)
	All(context.Context) (map[string][]Proxy, error)
	ByID(context.Context, string) (*Proxy, error)
	Healthy(context.Context, string) (bool, error)
	HealthyMany(context.Context, []Proxy) ([]Proxy, error)
	NextIndex(context.Context, string) (int64, error)
	GetAffinity(context.Context, string) (string, error)
	SetAffinity(context.Context, string, string, time.Duration) error
	RecordFailure(context.Context, string, int, time.Duration) (bool, error)
	RecordSuccess(context.Context, string) error
	RecordRequest(context.Context, string, string, bool, time.Duration, int64) error
	GetStats(context.Context, string) (Stats, error)
	GetStatsMany(context.Context, []string) ([]Stats, error)
	ProviderTotals(context.Context) (map[string]ProviderStats, error)
}

func Open(ctx context.Context, kind, redisURL, sqlitePath string) (DB, error) {
	switch strings.ToLower(kind) {
	case "", "redis":
		return NewRedis(ctx, redisURL)
	case "sqlite":
		return OpenSQLite(ctx, sqlitePath)
	default:
		return nil, fmt.Errorf("STORE: invalid value %q", kind)
	}
}
