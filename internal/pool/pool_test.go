package pool

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/store"
)

func testStore(t *testing.T) store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), "sqlite", "", filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRoundRobinAndValidAffinity(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	a := store.NewProxy("webshare", "a", 1, "", "")
	b := store.NewProxy("webshare", "b", 2, "", "")
	if err := db.Replace(ctx, "webshare", []store.Proxy{b, a}); err != nil {
		t.Fatal(err)
	}
	first, err := Select(ctx, db, []string{"webshare"}, config.RoundRobin, "session", 30*time.Minute)
	if err != nil || first == nil || first.ProxyID != a.ProxyID {
		t.Fatalf("initial selection = %v, %v", first, err)
	}
	next, err := Select(ctx, db, []string{"webshare"}, config.RoundRobin, "", 0)
	if err != nil || next.ProxyID != b.ProxyID {
		t.Fatalf("round robin = %v, %v", next, err)
	}
	if err := db.SetAffinity(ctx, "session", first.ProxyID, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	bound, err := Select(ctx, db, []string{"webshare"}, config.RoundRobin, "session", 30*time.Minute)
	if err != nil || bound.ProxyID != first.ProxyID {
		t.Fatalf("bound selection = %v, %v", bound, err)
	}
	unbound, err := Select(ctx, db, []string{"webshare"}, config.RoundRobin, "session", 0)
	if err != nil || unbound.ProxyID != a.ProxyID {
		t.Fatalf("disabled affinity = %v, %v", unbound, err)
	}
}

func TestInvalidBindUsesFirstHealthyInRouteOrder(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	first := store.NewProxy("webshare", "a", 1, "", "")
	second := store.NewProxy("webshare", "b", 2, "", "")
	other := store.NewProxy("rayobyte", "z", 3, "", "")
	if err := db.Replace(ctx, "webshare", []store.Proxy{second, first}); err != nil {
		t.Fatal(err)
	}
	if err := db.Replace(ctx, "rayobyte", []store.Proxy{other}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAffinity(ctx, "session", other.ProxyID, time.Minute); err != nil {
		t.Fatal(err)
	}
	selected, err := Select(ctx, db, []string{"webshare"}, config.RoundRobin, "session", time.Minute)
	if err != nil || selected.ProxyID != first.ProxyID {
		t.Fatalf("out-of-route bind = %v, %v", selected, err)
	}
	if err := db.SetAffinity(ctx, "session", first.ProxyID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordFailure(ctx, first.ProxyID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	selected, err = Select(ctx, db, []string{"webshare", "rayobyte"}, config.RoundRobin, "session", time.Minute)
	if err != nil || selected.ProxyID != second.ProxyID {
		t.Fatalf("unhealthy bind = %v, %v", selected, err)
	}
	if err := db.SetAffinity(ctx, "session", "webshare_gone_5", time.Minute); err != nil {
		t.Fatal(err)
	}
	selected, err = Select(ctx, db, []string{"webshare", "rayobyte"}, config.RoundRobin, "session", time.Minute)
	if err != nil || selected.ProxyID != second.ProxyID {
		t.Fatalf("missing bind = %v, %v", selected, err)
	}
}

func TestPriorityAndFallbackExcludeFailedProxy(t *testing.T) {
	ctx := context.Background()
	db := testStore(t)
	first := store.NewProxy("webshare", "a", 1, "", "")
	other := store.NewProxy("rayobyte", "b", 2, "", "")
	if err := db.Replace(ctx, "webshare", []store.Proxy{first}); err != nil {
		t.Fatal(err)
	}
	if err := db.Replace(ctx, "rayobyte", []store.Proxy{other}); err != nil {
		t.Fatal(err)
	}
	selected, err := Select(ctx, db, []string{"webshare", "rayobyte"}, config.PriorityFallback, "", 0)
	if err != nil || selected.ProxyID != first.ProxyID {
		t.Fatalf("priority = %v, %v", selected, err)
	}
	backup, err := Fallback(ctx, db, []string{"webshare", "rayobyte"}, first.ProxyID)
	if err != nil || backup.ProxyID != other.ProxyID {
		t.Fatalf("backup = %v, %v", backup, err)
	}
	if _, err := db.RecordFailure(ctx, other.ProxyID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	backup, err = Fallback(ctx, db, []string{"webshare", "rayobyte"}, first.ProxyID)
	if err != nil || backup.ProxyID != other.ProxyID {
		t.Fatalf("unhealthy backup = %v, %v", backup, err)
	}
	backup, err = Fallback(ctx, db, []string{"webshare"}, first.ProxyID)
	if err != nil || backup != nil {
		t.Fatalf("same-proxy fallback = %v, %v", backup, err)
	}
}
