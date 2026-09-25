package store

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	cmd := exec.Command(binary, "--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return "redis://" + address + "/0"
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Redis did not start")
	return ""
}

func TestRedisListsAndSharedHealth(t *testing.T) {
	ctx := context.Background()
	url := testRedis(t)
	first, err := NewRedis(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewRedis(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	proxy := Proxy{ProxyID: "webshare_127.0.0.1_8080", Provider: "webshare", Host: "127.0.0.1", Port: 8080}
	if err := first.Replace(ctx, "webshare", []Proxy{proxy}); err != nil {
		t.Fatal(err)
	}
	got, err := second.List(ctx, "webshare")
	if err != nil || len(got) != 1 || got[0].ProxyID != proxy.ProxyID {
		t.Fatalf("shared list: %v, %v", got, err)
	}
	byID, err := second.ByID(ctx, proxy.ProxyID)
	if err != nil || byID == nil || byID.Host != proxy.Host {
		t.Fatalf("lookup: %v, %v", byID, err)
	}
	for i := range 3 {
		marked, err := first.RecordFailure(ctx, proxy.ProxyID, 3, time.Minute)
		if err != nil || marked != (i == 2) {
			t.Fatalf("failure %d: marked=%v err=%v", i, marked, err)
		}
	}
	healthy, err := second.Healthy(ctx, proxy.ProxyID)
	if err != nil || healthy {
		t.Fatalf("shared health = %v, %v", healthy, err)
	}
	filtered, err := second.HealthyMany(ctx, []Proxy{proxy})
	if err != nil || len(filtered) != 0 {
		t.Fatalf("healthy filter = %v, %v", filtered, err)
	}
	if err := first.Replace(ctx, "webshare", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := second.List(ctx, "webshare"); err != nil || len(got) != 0 {
		t.Fatalf("removed list: %v, %v", got, err)
	}
	healthy, err = second.Healthy(ctx, proxy.ProxyID)
	if err != nil || !healthy {
		t.Fatalf("removed health = %v, %v", healthy, err)
	}
}

func TestRedisReplaceIsAtomicToReaders(t *testing.T) {
	ctx := context.Background()
	client, err := NewRedis(ctx, testRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a := Proxy{ProxyID: "webshare_a_1", Provider: "webshare", Host: "a", Port: 1}
	b := Proxy{ProxyID: "webshare_b_2", Provider: "webshare", Host: "b", Port: 2}
	if err := client.Replace(ctx, "webshare", []Proxy{a}); err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			got, err := client.List(ctx, "webshare")
			if err != nil {
				errors <- err
				return
			}
			if len(got) != 1 {
				errors <- fmt.Errorf("saw intermediate list: %d", len(got))
				return
			}
		}
	}()
	for range 200 {
		if err := client.Replace(ctx, "webshare", []Proxy{b}); err != nil {
			t.Fatal(err)
		}
		if err := client.Replace(ctx, "webshare", []Proxy{a}); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
}

func TestRedisIndexAndAffinityAcrossClients(t *testing.T) {
	ctx := context.Background()
	url := testRedis(t)
	first, err := NewRedis(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewRedis(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if n, err := first.NextIndex(ctx, "webshare"); err != nil || n != 1 {
		t.Fatalf("first index = %d, %v", n, err)
	}
	if n, err := second.NextIndex(ctx, "webshare"); err != nil || n != 2 {
		t.Fatalf("second index = %d, %v", n, err)
	}
	if err := first.SetAffinity(ctx, "session", "webshare_a_1", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if id, err := second.GetAffinity(ctx, "session"); err != nil || id != "webshare_a_1" {
		t.Fatalf("bind = %q, %v", id, err)
	}
	time.Sleep(150 * time.Millisecond)
	if id, err := second.GetAffinity(ctx, "session"); err != nil || id != "" {
		t.Fatalf("expired bind = %q, %v", id, err)
	}
}

func TestRedisStatsKeepLatestSamplesAndExpireOrphans(t *testing.T) {
	ctx := context.Background()
	client, err := NewRedis(ctx, testRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	p := NewProxy("webshare", "127.0.0.1", 8080, "", "")
	if err := client.Replace(ctx, "webshare", []Proxy{p}); err != nil {
		t.Fatal(err)
	}
	for i := range 1002 {
		if err := client.RecordRequest(ctx, p.ProxyID, p.Provider, true, time.Duration(i+1)*time.Millisecond, 1); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := client.GetStats(ctx, p.ProxyID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SuccessCount != 1002 || stats.BytesTransferred != 1002 || stats.LatencyCount != 1000 || stats.LatencyAvgMs != 502.5 || stats.LatencyP50Ms != 503 || stats.LatencyP95Ms != 953 {
		t.Fatalf("latest stats = %+v", stats)
	}
	if err := client.Replace(ctx, "webshare", nil); err != nil {
		t.Fatal(err)
	}
	if err := client.RecordRequest(ctx, p.ProxyID, p.Provider, false, time.Millisecond, 0); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stats:" + p.ProxyID, "latency:" + p.ProxyID} {
		ttl, err := client.client.TTL(ctx, key).Result()
		if err != nil || ttl < 29*24*time.Hour {
			t.Fatalf("orphan %s ttl = %s, %v", key, ttl, err)
		}
	}
}

func TestRedisReadsAndMigratesPythonLatency(t *testing.T) {
	ctx := context.Background()
	client, err := NewRedis(ctx, testRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	proxy := NewProxy("rayobyte", "127.0.0.1", 8080, "", "")
	if err := client.Replace(ctx, "rayobyte", []Proxy{proxy}); err != nil {
		t.Fatal(err)
	}
	statsKey, latencyKey := "stats:"+proxy.ProxyID, "latency:"+proxy.ProxyID
	if err := client.client.HSet(ctx, statsKey, "success", 2, "bytes", 40).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.client.ZAdd(ctx, latencyKey,
		redis.Z{Score: 20.5, Member: "1710000000.0"},
		redis.Z{Score: 10.25, Member: "1710000001.0"},
	).Err(); err != nil {
		t.Fatal(err)
	}
	stats, err := client.GetStats(ctx, proxy.ProxyID)
	if err != nil || stats.SuccessCount != 2 || stats.LatencyCount != 2 || stats.LatencyAvgMs != 15.375 {
		t.Fatalf("Python latency stats = %+v, %v", stats, err)
	}
	if err := client.RecordRequest(ctx, proxy.ProxyID, proxy.Provider, true, 3*time.Millisecond, 5); err != nil {
		t.Fatal(err)
	}
	stats, err = client.GetStats(ctx, proxy.ProxyID)
	if err != nil || stats.SuccessCount != 3 || stats.BytesTransferred != 45 || stats.LatencyCount != 3 ||
		stats.LatencyAvgMs != 11.25 || stats.LatencyP50Ms != 10.25 || stats.LatencyP95Ms != 20.5 {
		t.Fatalf("migrated latency stats = %+v, %v", stats, err)
	}
	kind, err := client.client.Type(ctx, latencyKey).Result()
	if err != nil || kind != "list" {
		t.Fatalf("migrated latency key type = %q, %v", kind, err)
	}
}

func TestRedisProviderTotalsSurviveRefresh(t *testing.T) {
	ctx := context.Background()
	url := testRedis(t)
	first, err := NewRedis(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewRedis(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	proxy := NewProxy("rayobyte", "127.0.0.1", 8080, "", "")
	if err := first.Replace(ctx, proxy.Provider, []Proxy{proxy}); err != nil {
		t.Fatal(err)
	}
	if err := first.RecordRequest(ctx, proxy.ProxyID, proxy.Provider, true, 10*time.Millisecond, 12); err != nil {
		t.Fatal(err)
	}
	if err := first.RecordRequest(ctx, proxy.ProxyID, proxy.Provider, false, 20*time.Millisecond, 3); err != nil {
		t.Fatal(err)
	}
	if err := first.Replace(ctx, proxy.Provider, nil); err != nil {
		t.Fatal(err)
	}
	replacement := NewProxy(proxy.Provider, "127.0.0.2", 8080, "", "")
	if err := first.Replace(ctx, proxy.Provider, []Proxy{replacement}); err != nil {
		t.Fatal(err)
	}
	if err := first.RecordRequest(ctx, replacement.ProxyID, replacement.Provider, true, 5*time.Millisecond, 7); err != nil {
		t.Fatal(err)
	}
	totals, err := second.ProviderTotals(ctx)
	want := ProviderStats{SuccessCount: 2, FailureCount: 1, BytesTransferred: 22, LatencyCount: 3, LatencyMicros: 35_000}
	if err != nil || totals[proxy.Provider] != want {
		t.Fatalf("provider totals after refresh = %v, %v", totals, err)
	}
}
func TestRedisGetStatsManyBatchesMixedLatencyTypes(t *testing.T) {
	ctx := context.Background()
	client, err := NewRedis(ctx, testRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	first := NewProxy("alpha", "127.0.0.1", 8001, "", "")
	second := NewProxy("beta", "127.0.0.2", 8002, "", "")
	if err := client.Replace(ctx, first.Provider, []Proxy{first}); err != nil {
		t.Fatal(err)
	}
	if err := client.Replace(ctx, second.Provider, []Proxy{second}); err != nil {
		t.Fatal(err)
	}
	if err := client.client.HSet(ctx, "stats:"+first.ProxyID, "success", 2, "failure", 1, "bytes", 40).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.client.LPush(ctx, "latency:"+first.ProxyID, "20000", "10000").Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.client.HSet(ctx, "stats:"+second.ProxyID, "success", 3, "bytes", 9).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.client.ZAdd(ctx, "latency:"+second.ProxyID,
		redis.Z{Score: 20.5, Member: "newer"},
		redis.Z{Score: 10.25, Member: "older"},
	).Err(); err != nil {
		t.Fatal(err)
	}
	ids := []string{second.ProxyID, "missing", first.ProxyID}
	got, err := client.GetStatsMany(ctx, ids)
	want := []Stats{
		{SuccessCount: 3, BytesTransferred: 9, LatencyCount: 2, LatencyAvgMs: 15.375, LatencyP50Ms: 20.5, LatencyP95Ms: 20.5},
		{},
		{SuccessCount: 2, FailureCount: 1, BytesTransferred: 40, LatencyCount: 2, LatencyAvgMs: 15, LatencyP50Ms: 20, LatencyP95Ms: 20},
	}
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stats[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := client.client.Set(ctx, "stats:"+first.ProxyID, "wrong type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetStatsMany(ctx, ids); err == nil {
		t.Fatal("GetStatsMany ignored a stats hash type error")
	}
}
