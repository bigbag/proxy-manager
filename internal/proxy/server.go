package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/pool"
	"github.com/bigbag/proxy-manager/internal/route"
	"github.com/bigbag/proxy-manager/internal/store"
)

type Server struct {
	Cfg    config.Settings
	Routes config.Routes
	Store  store.DB
	Dial   func(context.Context, string, string) (net.Conn, error)

	mu     sync.Mutex
	active map[net.Conn]net.Conn
}

func writeStatus(conn net.Conn, code int) error {
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", code, http.StatusText(code))
	return err
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	s.mu.Lock()
	s.active = make(map[net.Conn]net.Conn)
	s.mu.Unlock()
	slots := make(chan struct{}, s.Cfg.Proxy.MaxConnections)
	var workers sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
			s.closeActive()
		case <-stop:
		}
	}()
	defer close(stop)
	var acceptErr error
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			acceptErr = err
			break
		}
		select {
		case slots <- struct{}{}:
			s.mu.Lock()
			s.active[conn] = nil
			s.mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-slots }()
				s.Handle(ctx, conn)
				s.mu.Lock()
				delete(s.active, conn)
				s.mu.Unlock()
				_ = conn.Close()
			}()
		default:
			_ = writeStatus(conn, http.StatusServiceUnavailable)
			_ = conn.Close()
		}
	}
	_ = listener.Close()
	s.closeActive()
	workers.Wait()
	slog.Info("proxy stopped", "component", "proxy")
	return acceptErr
}

func (s *Server) closeActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for client, upstream := range s.active {
		_ = client.Close()
		if upstream != nil {
			_ = upstream.Close()
		}
	}
}

func (s *Server) record(p store.Proxy, success bool, latency time.Duration, bytes int64, unhealthy bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if unhealthy {
		marked, err := s.Store.RecordFailure(ctx, p.ProxyID, s.Cfg.Health.FailureThreshold, time.Duration(s.Cfg.Health.CooldownSeconds)*time.Second)
		if err != nil {
			slog.Error("health update failed", "component", "proxy", "proxy_id", p.ProxyID, "error", err)
		} else if marked {
			slog.Warn("proxy marked unhealthy", "component", "proxy", "proxy_id", p.ProxyID, "cooldown_seconds", s.Cfg.Health.CooldownSeconds)
		}
	} else if success {
		if err := s.Store.RecordSuccess(ctx, p.ProxyID); err != nil {
			slog.Error("health reset failed", "component", "proxy", "proxy_id", p.ProxyID, "error", err)
		}
	}
	if err := s.Store.RecordRequest(ctx, p.ProxyID, p.Provider, success, latency, bytes); err != nil {
		slog.Error("stats update failed", "component", "proxy", "proxy_id", p.ProxyID, "error", err)
	}
}

func (s *Server) Handle(ctx context.Context, conn net.Conn) {
	handshake, cancel := context.WithTimeout(ctx, time.Duration(s.Cfg.Proxy.ConnectTimeout)*time.Second)
	defer cancel()
	deadline, _ := handshake.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return
	}
	clientReader, host, port, key, status := readHead(conn)
	if status != 0 {
		_ = writeStatus(conn, status)
		return
	}
	slog.Info("CONNECT request", "component", "proxy", "host", host, "port", port)
	providers, strategy := route.Match(s.Routes, host)
	ttl := time.Duration(s.Cfg.Proxy.AffinityTTL) * time.Second
	p, err := pool.Select(handshake, s.Store, providers, strategy, key, ttl)
	if err != nil || p == nil {
		if err != nil {
			slog.Error("proxy selection failed", "component", "proxy", "host", host, "error", err)
		} else {
			slog.Warn("no proxy available", "component", "proxy", "host", host)
		}
		_ = writeStatus(conn, http.StatusBadGateway)
		return
	}
	var upstream net.Conn
	var upstreamReader *bufio.Reader
	var response *http.Response
	var elapsed time.Duration
	for attempt := range 2 {
		started := time.Now()
		upstream, upstreamReader, response, err = s.connectUpstream(handshake, *p, host, port)
		elapsed = time.Since(started)
		if err == nil {
			break
		}
		slog.Warn("upstream handshake failed", "component", "proxy", "proxy_id", p.ProxyID)
		s.record(*p, false, elapsed, 0, true)
		if attempt == 1 || key == "" || ttl <= 0 || handshake.Err() != nil {
			_ = writeStatus(conn, http.StatusBadGateway)
			return
		}
		next, nextErr := pool.Fallback(handshake, s.Store, providers, p.ProxyID)
		if nextErr != nil || next == nil {
			_ = writeStatus(conn, http.StatusBadGateway)
			return
		}
		p = next
	}
	slog.Info("upstream selected", "component", "proxy", "host", host, "port", port, "proxy_id", p.ProxyID)
	defer upstream.Close()
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if s.active != nil {
		s.active[conn] = upstream
	}
	s.mu.Unlock()
	if response.StatusCode != http.StatusOK {
		slog.Warn("upstream rejected CONNECT", "component", "proxy", "proxy_id", p.ProxyID, "status", response.StatusCode)
		if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			s.record(*p, false, elapsed, 0, false)
			return
		}
		if _, err := fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Type: %s\r\n\r\n", response.StatusCode, http.StatusText(response.StatusCode), response.Header.Get("Content-Type")); err == nil {
			if err := conn.SetWriteDeadline(deadline); err == nil {
				_, _ = io.CopyBuffer(conn, io.LimitReader(response.Body, 1<<20), make([]byte, 16<<10))
			}
		}
		s.record(*p, false, elapsed, 0, false)
		return
	}
	slog.Debug("upstream accepted CONNECT", "component", "proxy", "proxy_id", p.ProxyID)
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		s.record(*p, false, elapsed, 0, false)
		return
	}
	if _, err := fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		s.record(*p, false, elapsed, 0, false)
		return
	}
	if key != "" && ttl > 0 {
		if err := s.Store.SetAffinity(handshake, key, p.ProxyID, ttl); err != nil {
			slog.Error("affinity update failed", "component", "proxy", "proxy_id", p.ProxyID, "error", err)
		}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.record(*p, false, elapsed, 0, false)
		return
	}
	if err := upstream.SetDeadline(time.Time{}); err != nil {
		s.record(*p, false, elapsed, 0, true)
		return
	}
	bytes, failed := tunnel(conn, upstream, clientReader, upstreamReader, s.Cfg.Proxy.BufferSize)
	s.record(*p, !failed, elapsed, bytes, failed)
	slog.Debug("tunnel closed", "component", "proxy", "proxy_id", p.ProxyID, "bytes", bytes, "upstream_failed", failed)
}
