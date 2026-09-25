package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const failureScript = `
local count = redis.call('HINCRBY', KEYS[1], 'consecutive_failures', 1)
if redis.call('EXISTS', KEYS[2]) == 1 then return {count, 0} end
if count >= tonumber(ARGV[1]) then
  redis.call('SETEX', KEYS[2], tonumber(ARGV[2]), 'unhealthy')
  redis.call('HSET', KEYS[1], 'consecutive_failures', 0)
  return {count, 1}
end
return {count, 0}
`

const migrateLatencyScript = `
local key = KEYS[1]
if redis.call('TYPE', key).ok == 'zset' then
  local raw = redis.call('ZRANGE', key, 0, -1, 'WITHSCORES')
  local samples = {}
  for i = 1, #raw, 2 do
    samples[#samples + 1] = {tonumber(raw[i]), tonumber(raw[i + 1])}
  end
  table.sort(samples, function(a, b) return a[1] < b[1] end)
  redis.call('DEL', key)
  for _, sample in ipairs(samples) do
    redis.call('LPUSH', key, math.floor(sample[2] * 1000 + 0.5))
  end
end
redis.call('LPUSH', key, ARGV[1])
redis.call('LTRIM', key, 0, 999)
redis.call('EXPIRE', key, ARGV[2])
return 1
`

type Redis struct {
	client   *redis.Client
	failures *redis.Script
	migrate  *redis.Script
}

func NewRedis(ctx context.Context, url string) (*Redis, error) {
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_URL")
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &Redis{client: client, failures: redis.NewScript(failureScript), migrate: redis.NewScript(migrateLatencyScript)}, nil
}

func (s *Redis) Close() error                   { return s.client.Close() }
func (s *Redis) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

func (s *Redis) Replace(ctx context.Context, provider string, proxies []Proxy) error {
	previous, err := s.List(ctx, provider)
	if err != nil {
		return err
	}
	next := make(map[string]struct{}, len(proxies))
	fields := make([]any, 0, 2*len(proxies))
	for _, p := range proxies {
		// #nosec G117 -- The store needs credentials for upstream authentication.
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		fields = append(fields, p.ProxyID, string(raw))
		next[p.ProxyID] = struct{}{}
	}
	pipe := s.client.TxPipeline()
	pipe.Del(ctx, "proxies:"+provider)
	if len(fields) != 0 {
		pipe.HSet(ctx, "proxies:"+provider, fields...)
	}
	for _, p := range previous {
		if _, exists := next[p.ProxyID]; !exists {
			pipe.Del(ctx, "stats:"+p.ProxyID, "health:"+p.ProxyID, "latency:"+p.ProxyID)
		}
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *Redis) List(ctx context.Context, provider string) ([]Proxy, error) {
	values, err := s.client.HGetAll(ctx, "proxies:"+provider).Result()
	if err != nil {
		return nil, err
	}
	result := make([]Proxy, 0, len(values))
	for _, raw := range values {
		var p Proxy
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, nil
}

func (s *Redis) All(ctx context.Context) (map[string][]Proxy, error) {
	result := make(map[string][]Proxy)
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, "proxies:*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			provider := strings.TrimPrefix(key, "proxies:")
			list, err := s.List(ctx, provider)
			if err != nil {
				return nil, err
			}
			result[provider] = list
		}
		cursor = next
		if cursor == 0 {
			return result, nil
		}
	}
}

func (s *Redis) ByID(ctx context.Context, id string) (*Proxy, error) {
	provider, _, ok := strings.Cut(id, "_")
	if !ok {
		return nil, nil
	}
	raw, err := s.client.HGet(ctx, "proxies:"+provider, id).Result()
	if err == redis.Nil {
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

func (s *Redis) Healthy(ctx context.Context, id string) (bool, error) {
	count, err := s.client.Exists(ctx, "health:"+id).Result()
	return count == 0, err
}

func (s *Redis) HealthyMany(ctx context.Context, proxies []Proxy) ([]Proxy, error) {
	if len(proxies) == 0 {
		return nil, nil
	}
	pipe := s.client.Pipeline()
	commands := make([]*redis.IntCmd, len(proxies))
	for i, p := range proxies {
		commands[i] = pipe.Exists(ctx, "health:"+p.ProxyID)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	healthy := make([]Proxy, 0, len(proxies))
	for i, p := range proxies {
		count, err := commands[i].Result()
		if err != nil {
			return nil, err
		}
		if count == 0 {
			healthy = append(healthy, p)
		}
	}
	return healthy, nil
}

func (s *Redis) RecordFailure(ctx context.Context, id string, threshold int, cooldown time.Duration) (bool, error) {
	values, err := s.failures.Run(ctx, s.client, []string{"stats:" + id, "health:" + id}, threshold, int(cooldown.Seconds())).Slice()
	if err != nil {
		return false, err
	}
	if len(values) != 2 {
		return false, fmt.Errorf("invalid failure result: %v", values)
	}
	marked, ok := values[1].(int64)
	if !ok {
		return false, fmt.Errorf("invalid failure result: %v", values)
	}
	return marked == 1, nil
}

func (s *Redis) RecordSuccess(ctx context.Context, id string) error {
	return s.client.HSet(ctx, "stats:"+id, "consecutive_failures", 0).Err()
}

func (s *Redis) NextIndex(ctx context.Context, suffix string) (int64, error) {
	return s.client.Incr(ctx, "pool:rr_index:"+suffix).Result()
}

func (s *Redis) GetAffinity(ctx context.Context, key string) (string, error) {
	id, err := s.client.Get(ctx, "affinity:"+key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return id, err
}

func (s *Redis) SetAffinity(ctx context.Context, key, id string, ttl time.Duration) error {
	return s.client.Set(ctx, "affinity:"+key, id, ttl).Err()
}

func (s *Redis) RecordRequest(ctx context.Context, id, provider string, success bool, latency time.Duration, bytes int64) error {
	field := "failure"
	if success {
		field = "success"
	}
	statsKey, latencyKey := "stats:"+id, "latency:"+id
	providerKey := "provider_stats:" + provider
	pipe := s.client.TxPipeline()
	count := pipe.HIncrBy(ctx, statsKey, field, 1)
	bytesCount := pipe.HIncrBy(ctx, statsKey, "bytes", bytes)
	providerCount := pipe.HIncrBy(ctx, providerKey, field, 1)
	providerBytes := pipe.HIncrBy(ctx, providerKey, "bytes", bytes)
	providerLatencyCount := pipe.HIncrBy(ctx, providerKey, "latency_count", 1)
	providerLatencyMicros := pipe.HIncrBy(ctx, providerKey, "latency_micros", latency.Microseconds())
	push := pipe.LPush(ctx, latencyKey, strconv.FormatInt(latency.Microseconds(), 10))
	pipe.LTrim(ctx, latencyKey, 0, 999)
	statsTTL := pipe.Expire(ctx, statsKey, 30*24*time.Hour)
	latencyTTL := pipe.Expire(ctx, latencyKey, 30*24*time.Hour)
	_, err := pipe.Exec(ctx)
	if err != nil && count.Err() == nil && bytesCount.Err() == nil && providerCount.Err() == nil && providerBytes.Err() == nil &&
		providerLatencyCount.Err() == nil && providerLatencyMicros.Err() == nil &&
		statsTTL.Err() == nil && latencyTTL.Err() == nil && push.Err() != nil && strings.HasPrefix(push.Err().Error(), "WRONGTYPE ") {
		return s.migrate.Run(ctx, s.client, []string{latencyKey}, latency.Microseconds(), int64((30*24*time.Hour)/time.Second)).Err()
	}
	return err
}

func (s *Redis) GetStats(ctx context.Context, id string) (Stats, error) {
	stats, err := s.GetStatsMany(ctx, []string{id})
	if err != nil {
		return Stats{}, err
	}
	return stats[0], nil
}

func (s *Redis) GetStatsMany(ctx context.Context, ids []string) ([]Stats, error) {
	result := make([]Stats, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	pipe := s.client.Pipeline()
	hashes := make([]*redis.MapStringStringCmd, len(ids))
	samples := make([]*redis.StringSliceCmd, len(ids))
	for i, id := range ids {
		hashes[i] = pipe.HGetAll(ctx, "stats:"+id)
		samples[i] = pipe.LRange(ctx, "latency:"+id, 0, 999)
	}
	_, _ = pipe.Exec(ctx)
	legacyIDs := make([]int, 0)
	legacyCmds := make([]*redis.ZSliceCmd, 0)
	latencies := make([][]float64, len(ids))
	sums := make([]float64, len(ids))
	for i := range ids {
		fields, err := hashes[i].Result()
		if err != nil {
			return nil, err
		}
		result[i] = Stats{
			SuccessCount: parseCount(fields["success"]), FailureCount: parseCount(fields["failure"]),
			BytesTransferred: parseCount(fields["bytes"]),
		}
		raw, err := samples[i].Result()
		if err != nil {
			if !strings.HasPrefix(err.Error(), "WRONGTYPE ") {
				return nil, err
			}
			legacyIDs = append(legacyIDs, i)
			continue
		}
		latencies[i] = make([]float64, 0, len(raw))
		for _, sample := range raw {
			micros, err := strconv.ParseInt(sample, 10, 64)
			if err != nil {
				return nil, err
			}
			value := float64(micros) / 1000
			latencies[i] = append(latencies[i], value)
			sums[i] += value
		}
	}
	if len(legacyIDs) > 0 {
		legacyPipe := s.client.Pipeline()
		for _, i := range legacyIDs {
			legacyCmds = append(legacyCmds, legacyPipe.ZRangeWithScores(ctx, "latency:"+ids[i], 0, -1))
		}
		_, _ = legacyPipe.Exec(ctx)
		for j, cmd := range legacyCmds {
			legacy, err := cmd.Result()
			if err != nil {
				return nil, err
			}
			i := legacyIDs[j]
			latencies[i] = make([]float64, 0, len(legacy))
			for _, sample := range legacy {
				latencies[i] = append(latencies[i], sample.Score)
				sums[i] += sample.Score
			}
		}
	}
	for i, numbers := range latencies {
		sort.Float64s(numbers)
		result[i].LatencyCount = len(numbers)
		result[i].LatencyP50Ms = percentile(numbers, 50)
		result[i].LatencyP95Ms = percentile(numbers, 95)
		if len(numbers) > 0 {
			result[i].LatencyAvgMs = sums[i] / float64(len(numbers))
		}
	}
	return result, nil
}

func (s *Redis) ProviderTotals(ctx context.Context) (map[string]ProviderStats, error) {
	result := make(map[string]ProviderStats)
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, "provider_stats:*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			fields, err := s.client.HGetAll(ctx, key).Result()
			if err != nil {
				return nil, err
			}
			result[strings.TrimPrefix(key, "provider_stats:")] = ProviderStats{
				SuccessCount: parseCount(fields["success"]), FailureCount: parseCount(fields["failure"]),
				BytesTransferred: parseCount(fields["bytes"]),
				LatencyCount:     parseCount(fields["latency_count"]), LatencyMicros: parseCount(fields["latency_micros"]),
			}
		}
		cursor = next
		if cursor == 0 {
			return result, nil
		}
	}
}

func parseCount(value string) int64 {
	number, _ := strconv.ParseInt(value, 10, 64)
	return number
}

func percentile(sorted []float64, percent int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := len(sorted) * percent / 100
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
