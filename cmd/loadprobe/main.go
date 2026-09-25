package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/proxy"
	"github.com/bigbag/proxy-manager/internal/store"
	"github.com/redis/go-redis/v9"
)

func main() {
	redisURL := os.Getenv("REDIS_TEST_URL")
	var concurrency int
	var duration time.Duration
	var hold bool
	flag.IntVar(&concurrency, "concurrency", 1000, "simultaneous clients")
	flag.DurationVar(&duration, "duration", 30*time.Second, "probe duration")
	flag.BoolVar(&hold, "hold", false, "keep tunnels open")
	flag.Parse()
	if err := run(redisURL, concurrency, duration, hold); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(redisURL string, concurrency int, duration time.Duration, hold bool) error {
	parsed, err := url.Parse(redisURL)
	if err != nil || parsed.Scheme != "redis" || parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" || concurrency < 1 || duration <= 0 {
		return errors.New("use an isolated local Redis URL, positive concurrency, and positive duration")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return errors.New("invalid Redis URL")
	}
	client := redis.NewClient(options)
	defer client.Close()
	ctx := context.Background()
	keys, err := client.DBSize(ctx).Result()
	if err != nil {
		return err
	}
	if keys != 0 {
		return errors.New("test Redis database must be empty")
	}
	db, err := store.Open(ctx, "redis", redisURL, "")
	if err != nil {
		return err
	}
	defer db.Close()
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer upstream.Close()
	go func() {
		for {
			conn, err := upstream.Accept()
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
				if !hold {
					return
				}
				var data [1]byte
				for {
					if _, err := io.ReadFull(reader, data[:]); err != nil {
						return
					}
					if _, err := conn.Write(data[:]); err != nil {
						return
					}
				}
			}()
		}
	}()
	host, text, _ := net.SplitHostPort(upstream.Addr().String())
	port, _ := strconv.Atoi(text)
	if err := db.Replace(ctx, "loadprobe", []store.Proxy{store.NewProxy("loadprobe", host, port, "", "")}); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	server := &proxy.Server{Cfg: config.Settings{Proxy: config.ProxySettings{ConnectTimeout: 30, BufferSize: 16 << 10, MaxConnections: concurrency}, Health: config.HealthSettings{FailureThreshold: 3, CooldownSeconds: 300}}, Routes: config.Routes{Routes: []config.Route{{Pattern: "*", Providers: []string{"loadprobe"}, Strategy: config.RoundRobin}}}, Store: db}
	go func() { stopped <- server.Serve(serveCtx, listener) }()
	address := listener.Addr().String()
	baselineRSS, baselineFD, baselineGoroutines := metrics()
	baselineCommands := commandCount(client)
	var attempts, successes, failures, latencyMs atomic.Int64
	start := time.Now()
	end := start.Add(duration)
	var workers sync.WaitGroup
	sampleDone := make(chan struct{})
	sampled := make(chan [3]int64, 1)
	go func() {
		peak := [3]int64{baselineRSS, int64(baselineFD), int64(baselineGoroutines)}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rss, fds, goroutines := metrics()
				peak[0] = max(peak[0], rss)
				peak[1] = max(peak[1], int64(fds))
				peak[2] = max(peak[2], int64(goroutines))
			case <-sampleDone:
				sampled <- peak
				return
			}
		}
	}()
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for time.Now().Before(end) {
				attempts.Add(1)
				started := time.Now()
				var connectMs int64
				conn, err := net.DialTimeout("tcp", address, 5*time.Second)
				if err == nil {
					err = conn.SetDeadline(time.Now().Add(10 * time.Second))
					if err == nil {
						_, err = io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
					}
					if err == nil {
						reader := bufio.NewReader(conn)
						response, readErr := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
						err = readErr
						if err == nil && response.StatusCode != 200 {
							err = fmt.Errorf("CONNECT status %d", response.StatusCode)
						}
						if err == nil {
							connectMs = time.Since(started).Milliseconds()
						}
						if err == nil && hold {
							_, err = conn.Write([]byte{'x'})
							if err == nil {
								value, readErr := reader.ReadByte()
								err = readErr
								if err == nil && value != 'x' {
									err = errors.New("echo mismatch")
								}
							}
						}
					}
					if err == nil && hold {
						time.Sleep(time.Until(end))
					}
					_ = conn.Close()
				}
				if err != nil {
					if failures.Add(1) <= 5 {
						fmt.Fprintln(os.Stderr, "sample failure:", err)
					}
				} else {
					successes.Add(1)
					latencyMs.Add(connectMs)
				}
				if hold && err == nil {
					return
				}
			}
		}()
	}
	workers.Wait()
	close(sampleDone)
	peak := <-sampled
	elapsed := time.Since(start)
	cancel()
	if err := <-stopped; err != nil {
		return err
	}
	endRSS, endFD, endGoroutines := metrics()
	commands := commandCount(client) - baselineCommands
	fmt.Printf("hold=%t concurrency=%d duration=%s attempted=%d success=%d errors=%d requests_per_second=%.1f avg_connect_ms=%.1f rss_bytes=%d/%d/%d fds=%d/%d/%d goroutines=%d/%d/%d redis_commands=%d\n", hold, concurrency, elapsed, attempts.Load(), successes.Load(), failures.Load(), float64(attempts.Load())/elapsed.Seconds(), float64(latencyMs.Load())/float64(max(successes.Load(), 1)), baselineRSS, peak[0], endRSS, baselineFD, peak[1], endFD, baselineGoroutines, peak[2], endGoroutines, commands)
	return nil
}

func commandCount(client *redis.Client) int64 {
	info, err := client.Info(context.Background(), "commandstats").Result()
	if err != nil {
		return 0
	}
	var total int64
	for _, line := range strings.Split(info, "\n") {
		if !strings.HasPrefix(line, "cmdstat_") {
			continue
		}
		_, rest, ok := strings.Cut(line, "calls=")
		if !ok {
			continue
		}
		count, _, _ := strings.Cut(rest, ",")
		n, _ := strconv.ParseInt(count, 10, 64)
		total += n
	}
	return total
}

func metrics() (int64, int, int) {
	raw, _ := os.ReadFile("/proc/self/statm")
	var size, pages int64
	_, _ = fmt.Sscan(string(raw), &size, &pages)
	descriptors, _ := os.ReadDir("/proc/self/fd")
	return pages * int64(os.Getpagesize()), len(descriptors), runtime.NumGoroutine()
}
