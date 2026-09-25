package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bigbag/proxy-manager/internal/store"
)

func unusedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, text, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(text)
	return port
}

func startRedis(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("redis-server"); err != nil {
		t.Skip("redis-server unavailable")
	}
	port := unusedPort(t)
	command := exec.Command("redis-server", "--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Process.Kill(); command.Wait() })
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	waitForListener(t, address)
	return "redis://" + address + "/0"
}

func waitForListener(t *testing.T, address string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener %s not ready", address)
}

func startUpstream(t *testing.T, marker byte) store.Proxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				request, err := http.ReadRequest(reader)
				if err != nil || request.Method != http.MethodConnect {
					return
				}
				fmt.Fprintf(conn, "HTTP/1.1 200 Connection Established\r\n\r\n%c", marker)
				io.Copy(io.Discard, reader)
			}()
		}
	}()
	host, text, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(text)
	return store.NewProxy("webshare", host, port, "", "")
}

func buildService(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "proxy_manager")
	command := exec.Command("go", "build", "-o", binary, "./cmd/proxy_manager")
	command.Dir = "../.."
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %s: %v", output, err)
	}
	return binary
}

func startService(t *testing.T, binary, mode, kind, source, path, routes string) string {
	t.Helper()
	port := unusedPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, binary, "run", mode)
	command.Env = append(os.Environ(), "STORE="+kind, "REDIS_URL="+source, "SQLITE_PATH="+path, "ROUTES_CONFIG_PATH="+routes, "PROXY_HOST=127.0.0.1", "PROXY_PORT="+strconv.Itoa(port), "API_HOST=127.0.0.1", "API_PORT="+strconv.Itoa(port))
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); command.Wait() })
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	waitForListener(t, address)
	return address
}

func connectMarker(t *testing.T, address, key string) byte {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	header := ""
	if key != "" {
		header = "X-Proxy-Affinity: " + key + "\r\n"
	}
	if _, err := io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\n"+header+"\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT response = %v, %v", response, err)
	}
	marker, err := reader.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	return marker
}

func waitAffinity(t *testing.T, db store.DB, key, id string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		got, err := db.GetAffinity(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if got == id {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("affinity %q did not bind to %s", key, id)
}

func TestTwoNodesShareRedisAffinityHealthAndRefresh(t *testing.T) {
	redisURL := startRedis(t)
	ctx := context.Background()
	db, err := store.Open(ctx, "redis", redisURL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := startUpstream(t, 'A')
	b := startUpstream(t, 'B')
	proxies := []store.Proxy{a, b}
	if strings.Compare(a.ProxyID, b.ProxyID) > 0 {
		proxies[0], proxies[1] = b, a
	}
	if err := db.Replace(ctx, "webshare", proxies); err != nil {
		t.Fatal(err)
	}
	routePath := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(routePath, []byte(`{"routes":[{"pattern":"*","providers":["webshare"],"strategy":"round_robin"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	binary := buildService(t)
	nodeA := startService(t, binary, "proxy", "redis", redisURL, "", routePath)
	nodeB := startService(t, binary, "proxy", "redis", redisURL, "", routePath)
	marks := map[string]byte{a.ProxyID: 'A', b.ProxyID: 'B'}
	first := connectMarker(t, nodeA, "session")
	waitAffinity(t, db, "session", proxies[0].ProxyID)
	if first != marks[proxies[0].ProxyID] {
		t.Fatalf("first selected %c", first)
	}
	if got := connectMarker(t, nodeB, "session"); got != first {
		t.Fatalf("shared affinity = %c vs %c", first, got)
	}
	if got := connectMarker(t, nodeB, ""); got != marks[proxies[1].ProxyID] {
		t.Fatalf("shared round robin = %c", got)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		firstStats, err := db.GetStats(ctx, proxies[0].ProxyID)
		if err != nil {
			t.Fatal(err)
		}
		secondStats, err := db.GetStats(ctx, proxies[1].ProxyID)
		if err != nil {
			t.Fatal(err)
		}
		if firstStats.SuccessCount == 2 && secondStats.SuccessCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shared successes = %d and %d; want 2 and 1", firstStats.SuccessCount, secondStats.SuccessCount)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := db.RecordFailure(ctx, proxies[0].ProxyID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := connectMarker(t, nodeA, "different"); got != marks[proxies[1].ProxyID] {
		t.Fatalf("shared health = %c", got)
	}
	if err := db.Replace(ctx, "webshare", []store.Proxy{proxies[1]}); err != nil {
		t.Fatal(err)
	}
	removed, err := db.ByID(ctx, proxies[0].ProxyID)
	if err != nil || removed != nil {
		t.Fatalf("removed proxy = %v, %v", removed, err)
	}
	if got := connectMarker(t, nodeB, "session"); got != marks[proxies[1].ProxyID] {
		t.Fatalf("stale affinity = %c", got)
	}
}

func TestOneNodeSQLiteCONNECTAndAPI(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "proxy.db")
	db, err := store.Open(ctx, "sqlite", "", file)
	if err != nil {
		t.Fatal(err)
	}
	p := startUpstream(t, 'S')
	if err := db.Replace(ctx, "webshare", []store.Proxy{p}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	binary := buildService(t)
	proxyAddr := startService(t, binary, "proxy", "sqlite", "", file, filepath.Join(t.TempDir(), "absent.json"))
	if got := connectMarker(t, proxyAddr, ""); got != 'S' {
		t.Fatalf("SQLite CONNECT = %c", got)
	}
	apiAddr := startService(t, binary, "api", "sqlite", "", file, filepath.Join(t.TempDir(), "absent.json"))
	response, err := http.Get("http://" + apiAddr + "/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), `"healthy":1`) {
		t.Fatalf("SQLite API = %d, %s, %v", response.StatusCode, body, err)
	}
}
