package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
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

func TestTunnelRetriesHandshakeFailure(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		ttl       int
		failAll   bool
		status    int
	}{
		{"affinity", "session", 1800, false, 200},
		{"without affinity", "", 1800, false, 200},
		{"affinity disabled", "session", 0, false, 200},
		{"attempts exhausted", "", 0, true, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, func(c net.Conn) {
				reader := bufio.NewReader(c)
				if _, err := http.ReadRequest(reader); err != nil {
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				_, _ = io.Copy(c, reader)
			})
			s.Cfg.Proxy.AffinityTTL = tc.ttl
			failed := store.NewProxy("webshare", "a-failed", 8080, "", "")
			healthy := store.NewProxy("webshare", "b-healthy", 8081, "", "")
			extra := store.NewProxy("webshare", "c-extra", 8082, "", "")
			if err := s.Store.Replace(context.Background(), "webshare", []store.Proxy{failed, healthy, extra}); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			original := s.Dial
			s.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				calls.Add(1)
				if address == net.JoinHostPort(failed.Host, strconv.Itoa(failed.Port)) || tc.failAll {
					return nil, errors.New("refused")
				}
				return original(ctx, network, address)
			}
			request := "CONNECT example.com:443 HTTP/1.1\r\n"
			if tc.key != "" {
				request += "X-Proxy-Affinity: " + tc.key + "\r\n"
			}
			response, client := exchange(t, s, request+"\r\n")
			defer client.Close()
			if response.StatusCode != tc.status || calls.Load() != 2 {
				t.Fatalf("status = %d, attempts = %d", response.StatusCode, calls.Load())
			}
			if tc.status != 200 {
				return
			}
			if _, err := io.WriteString(client, "ping"); err != nil {
				t.Fatal(err)
			}
			var payload [4]byte
			if _, err := io.ReadFull(client, payload[:]); err != nil || string(payload[:]) != "ping" {
				t.Fatalf("fallback tunnel = %q, %v", payload, err)
			}
			bound, err := s.Store.GetAffinity(context.Background(), tc.key)
			want := ""
			if tc.key != "" && tc.ttl > 0 {
				want = healthy.ProxyID
			}
			if err != nil || bound != want {
				t.Fatalf("affinity = %q, want %q, error = %v", bound, want, err)
			}
		})
	}
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

func TestTunnelIdleTimeoutPreservesTrafficAndReleasesSlot(t *testing.T) {
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		_ = upstreamListener.Close()
	})
	go func() {
		for {
			conn, err := upstreamListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				if _, err := http.ReadRequest(reader); err != nil {
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					return
				}
				for range 20 {
					time.Sleep(100 * time.Millisecond)
					if _, err := conn.Write([]byte{'s'}); err != nil {
						return
					}
				}
				_, _ = io.Copy(io.Discard, reader)
				<-release
			}()
		}
	}()
	s := testServer(t, func(net.Conn) {})
	s.Dial = nil
	s.Cfg.Proxy.IdleTimeout = 1
	s.Cfg.Proxy.MaxConnections = 1
	s.Cfg.Health.FailureThreshold = 1
	s.Cfg.Health.CooldownSeconds = 60
	host, portText, _ := net.SplitHostPort(upstreamListener.Addr().String())
	port, _ := strconv.Atoi(portText)
	p := store.NewProxy("webshare", host, port, "", "")
	if err := s.Store.Replace(context.Background(), "webshare", []store.Proxy{p}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := conn.(*net.TCPConn)
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(client, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT = %v, %v", response, err)
	}
	for range 20 {
		if value, err := reader.ReadByte(); err != nil || value != 's' {
			t.Fatalf("upstream traffic = %q, %v", value, err)
		}
	}
	for range 20 {
		time.Sleep(100 * time.Millisecond)
		if _, err := client.Write([]byte{'c'}); err != nil {
			t.Fatalf("client traffic = %v", err)
		}
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("idle half-closed tunnel did not close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		stats, err := s.Store.GetStats(context.Background(), p.ProxyID)
		if err != nil {
			t.Fatal(err)
		}
		if stats.SuccessCount == 1 {
			if stats.FailureCount != 0 || stats.BytesTransferred != 40 {
				t.Fatalf("idle tunnel stats = %+v", stats)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle tunnel did not finish: %+v", stats)
		}
		time.Sleep(time.Millisecond)
	}
	if healthy, err := s.Store.Healthy(context.Background(), p.ProxyID); err != nil || !healthy {
		t.Fatalf("idle timeout marked upstream unhealthy: %v, %v", healthy, err)
	}
	replacement, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	_ = replacement.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(replacement, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	response, err = http.ReadResponse(bufio.NewReader(replacement), &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("released slot CONNECT = %v, %v", response, err)
	}
}

func TestTunnelPreservesUpstreamResetBeforeIdleExpiry(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var local, remote [2]*net.TCPConn
	for i := range local {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		peer, err := listener.Accept()
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		local[i], remote[i] = peer.(*net.TCPConn), conn.(*net.TCPConn)
		t.Cleanup(func() {
			conn.Close()
			peer.Close()
		})
	}
	done := make(chan bool, 1)
	go func() {
		_, failed := tunnel(local[0], local[1], bufio.NewReader(local[0]), bufio.NewReader(local[1]), 1024, 500*time.Millisecond)
		done <- failed
	}()
	if err := remote[1].SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := remote[1].Close(); err != nil {
		t.Fatal(err)
	}
	_ = remote[0].SetReadDeadline(time.Now().Add(2 * time.Second))
	var payload [1]byte
	if _, err := remote[0].Read(payload[:]); err != io.EOF {
		t.Fatalf("client read half did not close: %v", err)
	}
	select {
	case failed := <-done:
		if !failed {
			t.Fatal("idle expiry hid the upstream TCP reset")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle expiry did not close the client write half")
	}
}
