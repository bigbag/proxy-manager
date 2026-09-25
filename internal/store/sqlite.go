package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func (s *SQLite) GetStatsMany(ctx context.Context, ids []string) ([]Stats, error) {
	result := make([]Stats, len(ids))
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	const batchSize = 900
	values := make(map[string]Stats, len(unique))
	existing := make(map[string]bool, len(unique))
	latencies := make(map[string][]float64, len(unique))
	sums := make(map[string]float64, len(unique))
	for start := 0; start < len(unique); start += batchSize {
		end := min(start+batchSize, len(unique))
		batch := unique[start:end]
		// #nosec G202 -- placeholders returns only question marks. IDs are bound arguments.
		query := `SELECT id,success,failure,bytes FROM stats WHERE id IN (` + placeholders(len(batch)) + `)`
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var stats Stats
			if err := rows.Scan(&id, &stats.SuccessCount, &stats.FailureCount, &stats.BytesTransferred); err != nil {
				_ = rows.Close()
				return nil, err
			}
			values[id] = stats
			existing[id] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		query = `SELECT id,micros FROM (SELECT id,micros,ROW_NUMBER() OVER (PARTITION BY id ORDER BY seq DESC) AS n FROM latency WHERE id IN (` +
			placeholders(len(batch)) + `)) WHERE n<=1000 ORDER BY id,n`
		rows, err = s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var micros int64
			if err := rows.Scan(&id, &micros); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if existing[id] {
				sample := float64(micros) / 1000
				latencies[id] = append(latencies[id], sample)
				sums[id] += sample
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	for id, samples := range latencies {
		sort.Float64s(samples)
		stats := values[id]
		stats.LatencyCount = len(samples)
		stats.LatencyP50Ms = percentile(samples, 50)
		stats.LatencyP95Ms = percentile(samples, 95)
		stats.LatencyAvgMs = sums[id] / float64(len(samples))
		values[id] = stats
	}
	for i, id := range ids {
		result[i] = values[id]
	}
	return result, nil
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

type SQLite struct{ db *sql.DB }

func OpenSQLite(ctx context.Context, file string) (*SQLite, error) {
	paths, err := sqliteFiles(file)
	if err != nil {
		return nil, err
	}
	if err := secureSQLiteFiles(paths, false); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS proxies (id TEXT PRIMARY KEY, provider TEXT NOT NULL, data TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS proxies_provider ON proxies(provider)`,
		`CREATE TABLE IF NOT EXISTS health (id TEXT PRIMARY KEY, until INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS affinity (key TEXT PRIMARY KEY, proxy_id TEXT NOT NULL, until INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS affinity_until ON affinity(until)`,
		`CREATE TABLE IF NOT EXISTS rr_index (name TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS stats (id TEXT PRIMARY KEY, success INTEGER NOT NULL DEFAULT 0, failure INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0, consecutive_failures INTEGER NOT NULL DEFAULT 0, updated INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS provider_stats (provider TEXT PRIMARY KEY, success INTEGER NOT NULL DEFAULT 0, failure INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0, latency_count INTEGER NOT NULL DEFAULT 0, latency_micros INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS latency (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL, micros INTEGER NOT NULL, updated INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS latency_id_seq ON latency(id, seq)`,
		`CREATE INDEX IF NOT EXISTS latency_updated ON latency(updated)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	for _, migration := range []struct{ column, statement string }{
		{"latency_count", `ALTER TABLE provider_stats ADD COLUMN latency_count INTEGER NOT NULL DEFAULT 0`},
		{"latency_micros", `ALTER TABLE provider_stats ADD COLUMN latency_micros INTEGER NOT NULL DEFAULT 0`},
	} {
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('provider_stats') WHERE name=?`, migration.column).Scan(&exists); err != nil {
			_ = db.Close()
			return nil, err
		}
		if exists == 0 {
			if _, err := db.ExecContext(ctx, migration.statement); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	}
	if err := secureSQLiteFiles(paths, true); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

func sqliteFiles(file string) ([]string, error) {
	if file == "" || file == ":memory:" {
		return nil, nil
	}
	path := file
	if strings.HasPrefix(file, "file:") {
		u, err := url.Parse(file)
		if err != nil {
			return nil, fmt.Errorf("invalid SQLite path")
		}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return nil, fmt.Errorf("invalid SQLite path")
		}
		if query.Get("mode") == "memory" {
			return nil, nil
		}
		path = u.Path
		if u.Opaque != "" {
			path = u.Opaque
		}
		if path == "" || path == ":memory:" {
			return nil, nil
		}
	}
	paths := []string{path, path + "-wal", path + "-shm"}
	if target, err := filepath.EvalSymlinks(path); err == nil && target != path {
		paths = append(paths, target+"-wal", target+"-shm")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return paths, nil
}

func secureSQLiteFiles(paths []string, mainMustExist bool) error {
	for i, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && (i > 0 || !mainMustExist) {
			continue
		}
		if err != nil {
			return err
		}
		if i > 0 && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing SQLite sidecar symlink %q", filepath.Base(path))
		}
		info, err = os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("SQLite database files must be regular files")
		}
		mode := info.Mode().Perm()
		if mode&^os.FileMode(0600) != 0 {
			if err := os.Chmod(path, mode&0600); err != nil {
				return err
			}
			info, err = os.Stat(path)
			if err != nil {
				return err
			}
			if info.Mode().Perm()&^os.FileMode(0600) != 0 {
				return fmt.Errorf("SQLite database files are not private")
			}
		}
	}
	return nil
}

func (s *SQLite) Close() error                   { return s.db.Close() }
func (s *SQLite) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *SQLite) Replace(ctx context.Context, provider string, proxies []Proxy) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM proxies WHERE provider=?`, provider)
	if err != nil {
		return err
	}
	old := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		old = append(old, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	next := make(map[string]bool, len(proxies))
	if _, err := tx.ExecContext(ctx, `DELETE FROM proxies WHERE provider=?`, provider); err != nil {
		return err
	}
	for _, p := range proxies {
		// #nosec G117 -- The store needs credentials for upstream authentication.
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO proxies(id, provider, data) VALUES(?,?,?)`, p.ProxyID, provider, string(raw)); err != nil {
			return err
		}
		next[p.ProxyID] = true
	}
	for _, id := range old {
		if next[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM stats WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM health WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM latency WHERE id=?`, id); err != nil {
			return err
		}
	}
	now := time.Now().UnixNano()
	for _, statement := range []struct {
		query  string
		cutoff int64
	}{
		{`DELETE FROM affinity WHERE until<=?`, now},
		{`DELETE FROM health WHERE until<=?`, now},
		{`DELETE FROM stats WHERE updated<=?`, now - int64(30*24*time.Hour)},
		{`DELETE FROM latency WHERE updated<=?`, now - int64(30*24*time.Hour)},
	} {
		if _, err := tx.ExecContext(ctx, statement.query, statement.cutoff); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLite) List(ctx context.Context, provider string) ([]Proxy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM proxies WHERE provider=?`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Proxy
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p Proxy
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *SQLite) All(ctx context.Context) (map[string][]Proxy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, data FROM proxies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]Proxy)
	for rows.Next() {
		var provider, raw string
		if err := rows.Scan(&provider, &raw); err != nil {
			return nil, err
		}
		var p Proxy
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		result[provider] = append(result[provider], p)
	}
	return result, rows.Err()
}

func (s *SQLite) ByID(ctx context.Context, id string) (*Proxy, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM proxies WHERE id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Proxy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *SQLite) Healthy(ctx context.Context, id string) (bool, error) {
	var until int64
	err := s.db.QueryRowContext(ctx, `SELECT until FROM health WHERE id=?`, id).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return until <= time.Now().UnixNano(), err
}

func (s *SQLite) HealthyMany(ctx context.Context, proxies []Proxy) ([]Proxy, error) {
	result := make([]Proxy, 0, len(proxies))
	for _, p := range proxies {
		ok, err := s.Healthy(ctx, p.ProxyID)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, p)
		}
	}
	return result, nil
}

func (s *SQLite) NextIndex(ctx context.Context, name string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO rr_index(name,value) VALUES(?,1) ON CONFLICT(name) DO UPDATE SET value=value+1 RETURNING value`, name).Scan(&n)
	return n, err
}

func (s *SQLite) GetAffinity(ctx context.Context, key string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT proxy_id FROM affinity WHERE key=? AND until>?`, key, time.Now().UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *SQLite) SetAffinity(ctx context.Context, key, id string, ttl time.Duration) error {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM affinity WHERE until<=?`, now.UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO affinity(key,proxy_id,until) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET proxy_id=excluded.proxy_id, until=excluded.until`, key, id, now.Add(ttl).UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) RecordFailure(ctx context.Context, id string, threshold int, cooldown time.Duration) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var count int
	err = tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM stats WHERE id=?`, id).Scan(&count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	count++
	var until int64
	err = tx.QueryRowContext(ctx, `SELECT until FROM health WHERE id=?`, id).Scan(&until)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	now := time.Now()
	marked := until <= now.UnixNano() && count >= threshold
	if marked {
		if _, err := tx.ExecContext(ctx, `INSERT INTO health(id,until) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET until=excluded.until`, id, now.Add(cooldown).UnixNano()); err != nil {
			return false, err
		}
		count = 0
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO stats(id,consecutive_failures,updated) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET consecutive_failures=excluded.consecutive_failures,updated=excluded.updated`, id, count, now.UnixNano()); err != nil {
		return false, err
	}
	return marked, tx.Commit()
}

func (s *SQLite) RecordSuccess(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO stats(id,consecutive_failures,updated) VALUES(?,0,?) ON CONFLICT(id) DO UPDATE SET consecutive_failures=0,updated=excluded.updated`, id, time.Now().UnixNano())
	return err
}

func (s *SQLite) RecordRequest(ctx context.Context, id, provider string, success bool, latency time.Duration, bytes int64) error {
	successCount, failureCount := 0, 1
	if success {
		successCount, failureCount = 1, 0
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO stats(id,success,failure,bytes,updated) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET success=stats.success+excluded.success,failure=stats.failure+excluded.failure,bytes=stats.bytes+excluded.bytes,updated=excluded.updated`, id, successCount, failureCount, bytes, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO provider_stats(provider,success,failure,bytes,latency_count,latency_micros) VALUES(?,?,?,?,1,?) ON CONFLICT(provider) DO UPDATE SET success=provider_stats.success+excluded.success,failure=provider_stats.failure+excluded.failure,bytes=provider_stats.bytes+excluded.bytes,latency_count=provider_stats.latency_count+1,latency_micros=provider_stats.latency_micros+excluded.latency_micros`, provider, successCount, failureCount, bytes, latency.Microseconds()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO latency(id,micros,updated) VALUES(?,?,?)`, id, latency.Microseconds(), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM latency WHERE id=? AND seq NOT IN (SELECT seq FROM latency WHERE id=? ORDER BY seq DESC LIMIT 1000)`, id, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) GetStats(ctx context.Context, id string) (Stats, error) {
	var result Stats
	err := s.db.QueryRowContext(ctx, `SELECT success,failure,bytes FROM stats WHERE id=?`, id).Scan(&result.SuccessCount, &result.FailureCount, &result.BytesTransferred)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return Stats{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT micros FROM latency WHERE id=? ORDER BY seq DESC LIMIT 1000`, id)
	if err != nil {
		return Stats{}, err
	}
	defer rows.Close()
	var numbers []float64
	var sum float64
	for rows.Next() {
		var micros int64
		if err := rows.Scan(&micros); err != nil {
			return Stats{}, err
		}
		ms := float64(micros) / 1000
		numbers = append(numbers, ms)
		sum += ms
	}
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}
	sort.Float64s(numbers)
	result.LatencyCount = len(numbers)
	result.LatencyP50Ms = percentile(numbers, 50)
	result.LatencyP95Ms = percentile(numbers, 95)
	if len(numbers) > 0 {
		result.LatencyAvgMs = sum / float64(len(numbers))
	}
	return result, nil
}

func (s *SQLite) ProviderTotals(ctx context.Context) (map[string]ProviderStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider,success,failure,bytes,latency_count,latency_micros FROM provider_stats`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]ProviderStats)
	for rows.Next() {
		var provider string
		var stats ProviderStats
		if err := rows.Scan(&provider, &stats.SuccessCount, &stats.FailureCount, &stats.BytesTransferred, &stats.LatencyCount, &stats.LatencyMicros); err != nil {
			return nil, err
		}
		result[provider] = stats
	}
	return result, rows.Err()
}
