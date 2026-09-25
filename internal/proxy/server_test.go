package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/store"
)

func testServer(t *testing.T, reply func(net.Conn)) *Server {
	t.Helper()
	db, err := store.Open(context.Background(), "sqlite", "", filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	p := store.NewProxy("webshare", "upstream", 8080, "user", "password")
	if err := db.Replace(context.Background(), p.Provider, []store.Proxy{p}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: config.Settings{Proxy: config.ProxySettings{ConnectTimeout: 1, BufferSize: 16384, MaxConnections: 10}}, Routes: config.Routes{Routes: []config.Route{{Pattern: "*", Providers: []string{"webshare"}, Strategy: config.RoundRobin}}}, Store: db}
	s.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { defer b.Close(); reply(b) }()
		return a, nil
	}
	return s
}

func exchange(t *testing.T, s *Server, request string) (*http.Response, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); s.Handle(context.Background(), b); b.Close() }()
	t.Cleanup(func() { a.Close(); <-done })
	a.SetDeadline(time.Now().Add(4 * time.Second))
	go io.WriteString(a, request)
	response, err := http.ReadResponse(bufio.NewReader(a), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return response, a
}

func TestHeadRejectsInvalidRequests(t *testing.T) {
	s := testServer(t, func(net.Conn) {})
	for _, tc := range []struct {
		name, request string
		status        int
	}{
		{"other method", "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n", 405},
		{"bad syntax", "CONNECT /no-target HTTP/1.1\r\n\r\n", 400},
		{"bad port", "CONNECT example.com:0 HTTP/1.1\r\n\r\n", 400},
		{"large affinity", "CONNECT example.com:443 HTTP/1.1\r\nX-Proxy-Affinity: " + strings.Repeat("x", 257) + "\r\n\r\n", 400},
		{"large header", "CONNECT example.com:443 HTTP/1.1\r\nX-Extra: " + strings.Repeat("x", 65536) + "\r\n\r\n", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, conn := exchange(t, s, tc.request)
			defer conn.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("status = %d", response.StatusCode)
			}
			response.Body.Close()
		})
	}
}

func TestUpstreamCopiesStatusAndBoundsBody(t *testing.T) {
	s := testServer(t, func(c net.Conn) {
		request, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		if request.Method != http.MethodConnect || request.Host != "example.com:443" || request.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNzd29yZA==" || request.Header.Get("X-Proxy-Affinity") != "" {
			return
		}
		fmt.Fprintf(c, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: %d\r\nContent-Type: text/plain\r\n\r\n", 2<<20)
		io.WriteString(c, strings.Repeat("x", 2<<20))
	})
	response, conn := exchange(t, s, "CONNECT example.com:443 HTTP/1.1\r\nX-Proxy-Affinity: hello\r\n\r\n")
	defer conn.Close()
	if response.StatusCode != 407 {
		t.Fatalf("upstream status = %d", response.StatusCode)
	}
	if response.Header.Get("Content-Length") != "" {
		t.Fatalf("body length was forwarded: %v", response.Header)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || len(body) != 1<<20 {
		t.Fatalf("error body = %d bytes, %v", len(body), err)
	}
	response.Body.Close()
}

func TestUpstreamRejectsMalformedOrOversizedHead(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"not http", "garbage\r\n\r\n"},
		{"large head", "HTTP/1.1 200 OK\r\nX-Large: " + strings.Repeat("a", 65536) + "\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, func(c net.Conn) { http.ReadRequest(bufio.NewReader(c)); io.WriteString(c, tc.reply) })
			response, conn := exchange(t, s, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
			defer conn.Close()
			if response.StatusCode != 502 {
				t.Fatalf("status = %d", response.StatusCode)
			}
			response.Body.Close()
		})
	}
}

func TestUpstreamHandshakeDeadline(t *testing.T) {
	s := testServer(t, func(c net.Conn) { http.ReadRequest(bufio.NewReader(c)); io.Copy(io.Discard, c) })
	started := time.Now()
	response, conn := exchange(t, s, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	defer conn.Close()
	if response.StatusCode != 502 || time.Since(started) > 2*time.Second {
		t.Fatalf("status = %d after %v", response.StatusCode, time.Since(started))
	}
	response.Body.Close()
}

func TestProxyLogsConnectWithoutCredentials(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	s := testServer(t, func(c net.Conn) {
		_, _ = http.ReadRequest(bufio.NewReader(c))
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_, _ = io.Copy(io.Discard, c)
	})
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		s.Handle(context.Background(), server)
		_ = server.Close()
		close(done)
	}()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	go io.WriteString(client, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT response = %v, %v", response, err)
	}
	_ = client.Close()
	<-done
	logs := output.String()
	if !strings.Contains(logs, "level=INFO") || !strings.Contains(logs, "host=example.com") {
		t.Fatalf("CONNECT information missing: %q", logs)
	}
	selected := false
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=INFO") && strings.Contains(line, "proxy_id=webshare_upstream_8080") {
			selected = true
		}
	}
	if !selected {
		t.Fatalf("selected proxy ID missing from INFO logs: %q", logs)
	}
	if strings.Contains(logs, "password") || strings.Contains(logs, "dXNlcjpwYXNzd29yZA==") {
		t.Fatalf("CONNECT log exposes credentials: %q", logs)
	}
}
