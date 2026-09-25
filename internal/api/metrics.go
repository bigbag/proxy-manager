package api

import (
	"context"
	"slices"
	"strconv"

	"github.com/bigbag/proxy-manager/internal/store"
)

func appendMetricLabel(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for i := range len(value) {
		switch value[i] {
		case '\\', '"':
			dst = append(dst, '\\', value[i])
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, value[i])
		}
	}
	return append(dst, '"')
}

func appendMetricInt(dst []byte, name string, labels []byte, suffix string, value int64) []byte {
	dst = append(dst, name...)
	dst = append(dst, '{')
	dst = append(dst, labels...)
	dst = append(dst, suffix...)
	dst = append(dst, '}', ' ')
	dst = strconv.AppendInt(dst, value, 10)
	return append(dst, '\n')
}

func metrics(ctx context.Context, db store.DB) ([]byte, error) {
	all, err := db.All(ctx)
	if err != nil {
		return nil, err
	}
	totals, err := db.ProviderTotals(ctx)
	if err != nil {
		return nil, err
	}
	providers := make([]string, 0, len(all)+len(totals))
	for name := range all {
		providers = append(providers, name)
	}
	for name := range totals {
		if _, active := all[name]; !active {
			providers = append(providers, name)
		}
	}
	slices.Sort(providers)
	labels := make([][]byte, len(providers))
	out := make([]byte, 0, 1024+len(providers)*450)
	out = append(out, "# HELP proxy_manager_proxies The store counts current proxies by provider and health state.\n# TYPE proxy_manager_proxies gauge\n"...)
	for i, name := range providers {
		proxies := all[name]
		healthy, err := db.HealthyMany(ctx, proxies)
		if err != nil {
			return nil, err
		}
		labels[i] = appendMetricLabel(append(make([]byte, 0, 2*len(name)+12), "provider="...), name)
		out = appendMetricInt(out, "proxy_manager_proxies", labels[i], `,health="healthy"`, int64(len(healthy)))
		out = appendMetricInt(out, "proxy_manager_proxies", labels[i], `,health="unhealthy"`, int64(len(proxies)-len(healthy)))
	}
	out = append(out, "# HELP proxy_manager_requests_total The store counts CONNECT requests by provider and result.\n# TYPE proxy_manager_requests_total counter\n"...)
	for i, name := range providers {
		out = appendMetricInt(out, "proxy_manager_requests_total", labels[i], `,result="success"`, totals[name].SuccessCount)
		out = appendMetricInt(out, "proxy_manager_requests_total", labels[i], `,result="failure"`, totals[name].FailureCount)
	}
	out = append(out, "# HELP proxy_manager_bytes_transferred_total The store counts bytes copied in both tunnel directions by provider.\n# TYPE proxy_manager_bytes_transferred_total counter\n"...)
	for i, name := range providers {
		out = appendMetricInt(out, "proxy_manager_bytes_transferred_total", labels[i], "", totals[name].BytesTransferred)
	}
	out = append(out, "# HELP proxy_manager_connect_duration_seconds The upstream CONNECT handshake duration by provider in seconds.\n# TYPE proxy_manager_connect_duration_seconds summary\n"...)
	for i, name := range providers {
		out = append(out, "proxy_manager_connect_duration_seconds_sum{"...)
		out = append(out, labels[i]...)
		out = append(out, '}', ' ')
		out = strconv.AppendFloat(out, float64(totals[name].LatencyMicros)/1e6, 'f', -1, 64)
		out = append(out, '\n')
		out = appendMetricInt(out, "proxy_manager_connect_duration_seconds_count", labels[i], "", totals[name].LatencyCount)
	}
	return out, nil
}
