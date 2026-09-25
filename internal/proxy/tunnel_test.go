package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bigbag/proxy-manager/internal/store"
)

func TestTunnelTCPHalfCloseAndStats(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamListener.Close()
	go func() {
		c, err := upstreamListener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		reader := bufio.NewReader(c)
		if _, err := http.ReadRequest(reader); err != nil {
			return
		}
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		payload, _ := io.ReadAll(reader)
		if string(payload) == "ping" {
			io.WriteString(c, "pong")
		}
	}()
	s := testServer(t, func(net.Conn) {})
	s.Dial = nil
	host, portText, _ := net.SplitHostPort(upstreamListener.Addr().String())
	port, _ := strconv.Atoi(portText)
	proxy := store.NewProxy("webshare", host, port, "", "")
	if err := s.Store.Replace(context.Background(), "webshare", []store.Proxy{proxy}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- s.Serve(ctx, listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := conn.(*net.TCPConn)
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(client, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("status = %v, %v", response, err)
	}
	io.WriteString(client, "ping")
	client.CloseWrite()
	payload, err := io.ReadAll(reader)
	if err != nil || string(payload) != "pong" {
		t.Fatalf("after CloseWrite got %q, %v", payload, err)
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	stats, err := s.Store.GetStats(context.Background(), proxy.ProxyID)
	if err != nil || stats.SuccessCount != 1 || stats.BytesTransferred != 8 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

func TestTunnelRetriesOnlyWithAffinityBeforeResponse(t *testing.T) {
	s := testServer(t, func(c net.Conn) {
		http.ReadRequest(bufio.NewReader(c))
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		io.Copy(io.Discard, c)
	})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	s.Cfg.Proxy.AffinityTTL = 1800
	backup := store.NewProxy("webshare", "backup", 8081, "", "")
	first := store.NewProxy("webshare", "upstream", 8080, "", "")
	if err := s.Store.Replace(context.Background(), "webshare", []store.Proxy{first, backup}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		calls.Add(1)
		if address == net.JoinHostPort(backup.Host, strconv.Itoa(backup.Port)) {
			return nil, errors.New("refused")
		}
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			http.ReadRequest(bufio.NewReader(b))
			io.WriteString(b, "HTTP/1.1 200 Connection Established\r\n\r\n")
			io.Copy(io.Discard, b)
		}()
		return a, nil
	}
	response, client := exchange(t, s, "CONNECT example.com:443 HTTP/1.1\r\nX-Proxy-Affinity: session\r\n\r\n")
	if response.StatusCode != 200 || calls.Load() != 2 {
		t.Fatalf("status = %d, attempts = %d", response.StatusCode, calls.Load())
	}
	var bound string
	var err error
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		bound, err = s.Store.GetAffinity(context.Background(), "session")
		if bound != "" || err != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil || bound != first.ProxyID {
		t.Fatalf("bound = %q, %v", bound, err)
	}
	selected := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "level=INFO") {
			continue
		}
		if strings.Contains(line, "proxy_id="+backup.ProxyID) {
			t.Fatalf("logged failed proxy as selected: %q", line)
		}
		if strings.Contains(line, "proxy_id="+first.ProxyID) {
			selected = true
		}
	}
	if !selected {
		t.Fatalf("final proxy ID missing from INFO logs: %q", logs.String())
	}
	client.Close()
	if _, err := s.Store.NextIndex(context.Background(), "webshare"); err != nil {
		t.Fatal(err)
	}
	response, client = exchange(t, s, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	if response.StatusCode != 502 || calls.Load() != 3 {
		t.Fatalf("unbound status = %d, attempts = %d", response.StatusCode, calls.Load())
	}
	client.Close()
}

func TestTunnelDoesNotRetryValidUpstreamStatus(t *testing.T) {
	s := testServer(t, func(c net.Conn) {
		http.ReadRequest(bufio.NewReader(c))
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
	})
	s.Cfg.Proxy.AffinityTTL = 1800
	var calls atomic.Int32
	original := s.Dial
	s.Dial = func(ctx context.Context, n, a string) (net.Conn, error) { calls.Add(1); return original(ctx, n, a) }
	response, client := exchange(t, s, "CONNECT example.com:443 HTTP/1.1\r\nX-Proxy-Affinity: session\r\n\r\n")
	defer client.Close()
	if response.StatusCode != 407 || calls.Load() != 1 {
		t.Fatalf("status = %d, attempts = %d", response.StatusCode, calls.Load())
	}
	bound, err := s.Store.GetAffinity(context.Background(), "session")
	if err != nil || bound != "" {
		t.Fatalf("unexpected affinity = %q, %v", bound, err)
	}
}

func TestTunnelCapacityAndShutdown(t *testing.T) {
	s := testServer(t, func(net.Conn) {})
	s.Cfg.Proxy.MaxConnections = 1
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	first, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(second), nil)
	if err != nil || response.StatusCode != 503 {
		t.Fatalf("capacity = %v, %v", response, err)
	}
	response.Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	first.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if _, err := first.Read(buf[:]); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("active client not closed: %v", err)
	}
}
