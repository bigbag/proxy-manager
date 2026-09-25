package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSQLiteStatePersistsAndExpires(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "proxy.db")
	db, err := Open(ctx, "sqlite", "", path)
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy("webshare", "127.0.0.1", 8080, "u", "p")
	if err := db.Replace(ctx, "webshare", []Proxy{proxy}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAffinity(ctx, "session", proxy.ProxyID, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, "sqlite", "", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if id, err := db.GetAffinity(ctx, "session"); err != nil || id != proxy.ProxyID {
		t.Fatalf("persisted bind = %q, %v", id, err)
	}
	if got, err := db.ByID(ctx, proxy.ProxyID); err != nil || got == nil || got.Password != "p" {
		t.Fatalf("persisted proxy = %v, %v", got, err)
	}
	if all, err := db.All(ctx); err != nil || len(all["webshare"]) != 1 {
		t.Fatalf("all = %v, %v", all, err)
	}
	for i := range 3 {
		marked, err := db.RecordFailure(ctx, proxy.ProxyID, 3, 100*time.Millisecond)
		if err != nil || marked != (i == 2) {
			t.Fatalf("failure %d = %v, %v", i, marked, err)
		}
	}
	if ok, err := db.Healthy(ctx, proxy.ProxyID); err != nil || ok {
		t.Fatalf("cooldown = %v, %v", ok, err)
	}
	time.Sleep(250 * time.Millisecond)
	if id, err := db.GetAffinity(ctx, "session"); err != nil || id != "" {
		t.Fatalf("expired bind = %q, %v", id, err)
	}
	if ok, err := db.Healthy(ctx, proxy.ProxyID); err != nil || !ok {
		t.Fatalf("recovered health = %v, %v", ok, err)
	}
	if err := db.Replace(ctx, "webshare", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := db.ByID(ctx, proxy.ProxyID); err != nil || got != nil {
		t.Fatalf("removed proxy = %v, %v", got, err)
	}
}

func TestSQLiteConcurrentIndexesAndBoundedStats(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, "sqlite", "", filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var wg sync.WaitGroup
	seen := make(chan int64, 40)
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := db.NextIndex(ctx, "webshare")
			if err != nil {
				t.Error(err)
				return
			}
			seen <- n
		}()
	}
	wg.Wait()
	close(seen)
	unique := map[int64]bool{}
	for n := range seen {
		unique[n] = true
	}
	if len(unique) != 40 {
		t.Fatalf("counter values = %v", unique)
	}
	for i := range 1002 {
		if err := db.RecordRequest(ctx, "webshare_a_1", "webshare", true, time.Duration(i+1)*time.Millisecond, 1); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.GetStats(ctx, "webshare_a_1")
	if err != nil || got.SuccessCount != 1002 || got.BytesTransferred != 1002 || got.LatencyCount != 1000 || got.LatencyP50Ms != 503 {
		t.Fatalf("bounded stats = %+v, %v", got, err)
	}
	if unknown, err := db.GetStats(ctx, "missing"); err != nil || unknown != (Stats{}) {
		t.Fatalf("unknown = %+v, %v", unknown, err)
	}
}

func TestOpenRejectsUnknownStore(t *testing.T) {
	if _, err := Open(context.Background(), "other", "", ""); err == nil {
		t.Fatal("accepted unknown store")
	}
}

func TestSQLiteProviderTotalsSurviveRefreshAndReopen(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "proxy.db")
	db, err := OpenSQLite(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy("webshare", "127.0.0.1", 8080, "", "")
	if err := db.Replace(ctx, proxy.Provider, []Proxy{proxy}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRequest(ctx, proxy.ProxyID, proxy.Provider, true, 10*time.Millisecond, 12); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRequest(ctx, proxy.ProxyID, proxy.Provider, false, 20*time.Millisecond, 3); err != nil {
		t.Fatal(err)
	}
	if err := db.Replace(ctx, proxy.Provider, nil); err != nil {
		t.Fatal(err)
	}
	replacement := NewProxy(proxy.Provider, "127.0.0.2", 8080, "", "")
	if err := db.Replace(ctx, proxy.Provider, []Proxy{replacement}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRequest(ctx, replacement.ProxyID, replacement.Provider, true, 5*time.Millisecond, 7); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenSQLite(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	totals, err := db.ProviderTotals(ctx)
	want := ProviderStats{SuccessCount: 2, FailureCount: 1, BytesTransferred: 22, LatencyCount: 3, LatencyMicros: 35_000}
	if err != nil || totals[proxy.Provider] != want {
		t.Fatalf("provider totals after reopen = %v, %v", totals, err)
	}
}

func TestSQLiteMigratesProviderLatencyCounters(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE provider_stats (provider TEXT PRIMARY KEY, success INTEGER NOT NULL DEFAULT 0, failure INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO provider_stats(provider,success,failure,bytes) VALUES('webshare',5,2,70)`,
	} {
		if _, err := old.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenSQLite(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy("webshare", "127.0.0.1", 8080, "", "")
	if err := db.RecordRequest(ctx, proxy.ProxyID, proxy.Provider, true, 40*time.Millisecond, 4); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenSQLite(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	totals, err := db.ProviderTotals(ctx)
	want := ProviderStats{SuccessCount: 6, FailureCount: 2, BytesTransferred: 74, LatencyCount: 1, LatencyMicros: 40_000}
	if err != nil || totals[proxy.Provider] != want {
		t.Fatalf("migrated provider totals = %v, %v", totals, err)
	}
}

func TestSQLiteFilesArePrivate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "proxy.db")
	if err := os.WriteFile(path, nil, 0666); err != nil {
		t.Fatal(err)
	}
	checkPrivate := func(dbPath string) {
		t.Helper()
		for _, file := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			info, err := os.Stat(file)
			if err != nil {
				t.Fatalf("stat %s: %v", filepath.Base(file), err)
			}
			if info.Mode().Perm()&0077 != 0 {
				t.Errorf("%s mode = %04o", filepath.Base(file), info.Mode().Perm())
			}
		}
	}
	db, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	checkPrivate(path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	db, err = OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkPrivate(path)
	newPath := filepath.Join(t.TempDir(), "new.db")
	newDB, err := OpenSQLite(ctx, newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer newDB.Close()
	checkPrivate(newPath)
}

func TestSQLiteGetStatsManyMatchesDetailReads(t *testing.T) {
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	proxies := []Proxy{
		NewProxy("alpha", "a", 1, "", ""),
		NewProxy("beta", "b", 2, "", ""),
	}
	for _, p := range proxies {
		if err := db.Replace(ctx, p.Provider, []Proxy{p}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sample := range []struct {
		id      string
		success bool
		latency time.Duration
		bytes   int64
	}{
		{proxies[0].ProxyID, true, 10 * time.Millisecond, 30},
		{proxies[0].ProxyID, false, 20 * time.Millisecond, 20},
		{proxies[1].ProxyID, true, 7 * time.Millisecond, 9},
	} {
		provider := "alpha"
		if sample.id == proxies[1].ProxyID {
			provider = "beta"
		}
		if err := db.RecordRequest(ctx, sample.id, provider, sample.success, sample.latency, sample.bytes); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{proxies[1].ProxyID, "missing", proxies[0].ProxyID}
	got, err := db.GetStatsMany(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		want, err := db.GetStats(ctx, id)
		if err != nil || got[i] != want {
			t.Fatalf("stats for %q = %+v, want %+v, %v", id, got[i], want, err)
		}
	}
	if _, err := db.GetStatsMany(ctx, []string{"missing"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetStatsMany(ctx, ids); err == nil {
		t.Fatal("GetStatsMany succeeded after close")
	}
}
