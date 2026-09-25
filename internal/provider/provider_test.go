package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bigbag/proxy-manager/internal/store"
)

func TestWebshareFetchesPagesAndSkipsInvalid(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/list/" || r.URL.Query().Get("mode") != "direct" || r.URL.Query().Get("page_size") != "100" || r.Header.Get("Authorization") != "token" {
			t.Errorf("invalid Webshare request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"results":[{"valid":true,"proxy_address":"127.0.0.1","port":8080,"username":"u","password":"p"},{"valid":false,"proxy_address":"bad","port":1}],"next":"more"}`)
		} else {
			fmt.Fprint(w, `{"results":[{"valid":true,"proxy_address":"127.0.0.1","port":"8080","username":"u","password":"p"},{"valid":true,"proxy_address":"bad","port":0}],"next":null}`)
		}
	}))
	defer server.Close()
	got, err := (&Webshare{BaseURL: server.URL, APIKey: "token"}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(got) != 1 || got[0].ProxyID != "webshare_127.0.0.1_8080" || got[0].Password != "p" {
		t.Fatalf("Webshare pages = %d, %+v", calls, got)
	}
}

func TestRayobyteParsesCSVAndHidesCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/export/4/all/login/key/list.csv") || r.URL.Query().Get("additionalValues") != "password" {
			t.Errorf("Rayobyte request: %s", r.URL.Path)
		}
		fmt.Fprint(w, "127.0.0.1:8080:u:p\ninvalid\n127.0.0.2:nope:u:p\n")
	}))
	defer server.Close()
	got, err := (&Rayobyte{BaseURL: server.URL, APILogin: "login", APIKey: "key"}).Fetch(context.Background())
	if err != nil || len(got) != 1 || got[0].Host != "127.0.0.1" {
		t.Fatalf("Rayobyte = %+v, %v", got, err)
	}
}

func TestRayobyteRejectsOversizedValidCSV(t *testing.T) {
	const maxExportSize = 8 << 20
	prefix := "127.0.0.1:80:u:p:"
	boundary := prefix + strings.Repeat("x", maxExportSize-len(prefix)-1) + "\n"
	if len(boundary) != maxExportSize {
		t.Fatalf("boundary size = %d", len(boundary))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, boundary, "127.0.0.2:81:u:p\n")
	}))
	defer server.Close()

	got, err := (&Rayobyte{BaseURL: server.URL, APILogin: "login", APIKey: "key"}).Fetch(context.Background())
	if err == nil || got != nil {
		t.Fatalf("oversized Rayobyte CSV = %+v, %v", got, err)
	}
}

func TestProxyRackFailedProbeDoesNotReturnEmptySuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	f := &ProxyRack{Address: "127.0.0.1", InitialPort: port, Slots: 2, APILogin: "u", APIKey: "p"}
	got, err := f.Fetch(context.Background())
	if err != nil || len(got) != 2 || got[1].Port != port+1 {
		t.Fatalf("ProxyRack = %+v, %v", got, err)
	}
	ln.Close()
	if _, err := f.Fetch(context.Background()); err == nil {
		t.Fatal("failed probe replaced previous list")
	}
}

type fakeFetcher struct {
	list     []store.Proxy
	err      error
	interval time.Duration
}

func (f *fakeFetcher) Name() string                                 { return "webshare" }
func (f *fakeFetcher) Interval() time.Duration                      { return f.interval }
func (f *fakeFetcher) Fetch(context.Context) ([]store.Proxy, error) { return f.list, f.err }

func TestRefresherKeepsPreviousListOnFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, "sqlite", "", filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	original := store.NewProxy("webshare", "127.0.0.1", 8080, "", "")
	if err := db.Replace(ctx, "webshare", []store.Proxy{original}); err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1000, 0)
	fetcher := &fakeFetcher{err: errors.New("provider down"), interval: time.Minute}
	r := &Refresher{Store: db, Fetchers: []Fetcher{fetcher}, Now: func() time.Time { return clock }}
	if count := r.RefreshAll(ctx)["webshare"]; count != 0 {
		t.Fatalf("failed refresh count = %d", count)
	}
	if got, err := db.List(ctx, "webshare"); err != nil || len(got) != 1 {
		t.Fatalf("previous list = %v, %v", got, err)
	}
	fetcher.err = nil
	fetcher.list = nil
	if count := r.RefreshAll(ctx)["webshare"]; count != 0 {
		t.Fatalf("empty successful fetch count = %d", count)
	}
	if got, err := db.List(ctx, "webshare"); err != nil || len(got) != 0 {
		t.Fatalf("empty list = %v, %v", got, err)
	}
	if r.Needs(fetcher) {
		t.Fatal("successful fetch did not update interval")
	}
	clock = clock.Add(time.Minute)
	if !r.Needs(fetcher) {
		t.Fatal("due provider was skipped")
	}
}
