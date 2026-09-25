package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandRejectsInvalidArguments(t *testing.T) {
	t.Setenv("STORE", "sqlite")
	t.Setenv("SQLITE_PATH", filepath.Join(t.TempDir(), "proxy.db"))
	for _, args := range [][]string{nil, {"run"}, {"run", "other"}, {"refresh", "--unknown"}, {"stats", "extra"}, {"other"}} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("accepted invalid args: %v", args)
		}
	}
}

func TestRefreshOnceAndReadCommands(t *testing.T) {
	t.Setenv("STORE", "sqlite")
	t.Setenv("SQLITE_PATH", filepath.Join(t.TempDir(), "proxy.db"))
	for _, args := range [][]string{{"refresh", "--once"}, {"stats"}, {"list-proxies"}} {
		if err := run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestRefreshLogsUsefulInformationWithoutSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "provider-secret-123" {
			t.Error("Webshare request omitted authorization")
		}
		_, _ = io.WriteString(w, `{"results":[{"valid":true,"proxy_address":"127.0.0.1","port":8080,"username":"user","password":"upstream-secret-456"}]}`)
	}))
	defer server.Close()
	t.Setenv("STORE", "sqlite")
	t.Setenv("SQLITE_PATH", filepath.Join(t.TempDir(), "proxy.db"))
	file := filepath.Join(t.TempDir(), "providers.json")
	t.Setenv("PROVIDERS_CONFIG_PATH", file)
	if err := os.WriteFile(file, []byte(`{"webshare":{"base_url":"`+server.URL+`","api_key":"provider-secret-123"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	previousLogger, previousStderr := slog.Default(), os.Stderr
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		os.Stderr = previousStderr
	})
	capture := func(level string) string {
		t.Setenv("LOG_LEVEL", level)
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		defer writer.Close()
		os.Stderr = writer
		if err := run(context.Background(), []string{"refresh", "--once"}); err != nil {
			t.Fatal(err)
		}
		os.Stderr = previousStderr
		_ = writer.Close()
		output, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		return string(output)
	}
	info := capture("INFO")
	if !strings.Contains(info, "level=INFO") || !strings.Contains(info, "provider=webshare") || !strings.Contains(info, "count=1") {
		t.Fatalf("refresh information missing: %q", info)
	}
	if strings.Contains(info, "provider-secret-123") || strings.Contains(info, "upstream-secret-456") {
		t.Fatalf("refresh log exposes credentials: %q", info)
	}
	if quiet := capture("ERROR"); strings.Contains(quiet, "level=INFO") {
		t.Fatalf("LOG_LEVEL did not filter information: %q", quiet)
	}
}

func TestMakeRefreshLoadsLocalEnv(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"valid":true,"proxy_address":"127.0.0.1","port":8080,"username":"user","password":"upstream-secret"}],"next":null}`)
	}))
	defer server.Close()

	configFile := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(configFile, []byte(`{"webshare":{"base_url":"`+server.URL+`","api_key":"provider-secret"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), ".local_env")
	localEnv := "PROVIDERS_CONFIG_PATH=" + configFile + "\n"
	if err := os.WriteFile(file, []byte(localEnv), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("make", "--no-print-directory", "refresh/once")
	command.Dir = filepath.Join("..", "..")
	command.Env = append(os.Environ(), "STORE=sqlite", "SQLITE_PATH="+filepath.Join(t.TempDir(), "proxy.db"),
		"LOCAL_ENV_FILE="+file, "PROVIDERS_CONFIG_PATH="+filepath.Join(t.TempDir(), "missing.json"), "LOG_LEVEL=INFO")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("make refresh/once: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "providers=1") || !strings.Contains(string(output), "provider=webshare count=1") {
		t.Fatalf("refresh did not use local provider settings: %s", output)
	}
	if strings.Contains(string(output), "provider-secret") || strings.Contains(string(output), "upstream-secret") {
		t.Fatalf("refresh output exposes credentials: %s", output)
	}
}

func TestRefreshRejectsUnsupportedProvider(t *testing.T) {
	t.Setenv("STORE", "sqlite")
	t.Setenv("SQLITE_PATH", filepath.Join(t.TempDir(), "proxy.db"))
	file := filepath.Join(t.TempDir(), "providers.json")
	t.Setenv("PROVIDERS_CONFIG_PATH", file)
	for _, data := range []string{`{"other":{}}`, `{"proxyrack":{"initial_port":65535,"slots":2}}`} {
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(context.Background(), []string{"refresh", "--once"}); err == nil {
			t.Fatalf("accepted invalid provider configuration %s", data)
		}
	}
}
