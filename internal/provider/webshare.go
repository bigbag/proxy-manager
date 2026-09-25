package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bigbag/proxy-manager/internal/store"
)

type Webshare struct {
	BaseURL, APIKey string
	Every           time.Duration
	Client          *http.Client
}

func (w *Webshare) Name() string { return "webshare" }
func (w *Webshare) Interval() time.Duration {
	if w.Every > 0 {
		return w.Every
	}
	return 30 * time.Minute
}

func (w *Webshare) Fetch(ctx context.Context) ([]store.Proxy, error) {
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	seen := make(map[string]bool)
	result := make([]store.Proxy, 0)
	for page := 1; ; page++ {
		if page > 1 {
			timer := time.NewTimer(3 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		endpoint, err := url.Parse(strings.TrimRight(w.BaseURL, "/") + "/list/")
		if err != nil {
			return nil, fmt.Errorf("invalid Webshare address")
		}
		query := endpoint.Query()
		query.Set("mode", "direct")
		query.Set("ordering", "id")
		query.Set("page_size", "100")
		query.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("invalid Webshare request")
		}
		request.Header.Set("Authorization", w.APIKey)
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("Webshare request failed")
		}
		var data struct {
			Results []struct {
				Valid    bool            `json:"valid"`
				Address  string          `json:"proxy_address"`
				Port     json.RawMessage `json:"port"`
				User     string          `json:"username"`
				Password string          `json:"password"`
			} `json:"results"`
			Next json.RawMessage `json:"next"`
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return nil, fmt.Errorf("Webshare status %d", response.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&data)
		_ = response.Body.Close()
		if err != nil || data.Results == nil {
			return nil, fmt.Errorf("invalid Webshare response")
		}
		for _, item := range data.Results {
			if !item.Valid || item.Address == "" {
				continue
			}
			port, err := strconv.Atoi(strings.Trim(string(item.Port), `"`))
			if err != nil || port < 1 || port > 65535 {
				continue
			}
			proxy := store.NewProxy(w.Name(), item.Address, port, item.User, item.Password)
			if !seen[proxy.ProxyID] {
				result = append(result, proxy)
				seen[proxy.ProxyID] = true
			}
		}
		if len(data.Next) == 0 || string(data.Next) == "null" {
			return result, nil
		}
	}
}
