package provider

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bigbag/proxy-manager/internal/store"
)

type Rayobyte struct {
	BaseURL, APILogin, APIKey string
	Every                     time.Duration
	Client                    *http.Client
}

func (r *Rayobyte) Name() string { return "rayobyte" }
func (r *Rayobyte) Interval() time.Duration {
	if r.Every > 0 {
		return r.Every
	}
	return 30 * time.Minute
}

func (r *Rayobyte) Fetch(ctx context.Context) ([]store.Proxy, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	endpoint, err := url.Parse(strings.TrimRight(r.BaseURL, "/") + "/export/4/all/" + url.PathEscape(r.APILogin) + "/" + url.PathEscape(r.APIKey) + "/list.csv")
	if err != nil {
		return nil, fmt.Errorf("invalid Rayobyte address")
	}
	query := endpoint.Query()
	query.Set("additionalValues", "password")
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid Rayobyte request")
	}
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Rayobyte request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Rayobyte status %d", response.StatusCode)
	}
	const maxExportSize = 8 << 20
	limited := &io.LimitedReader{R: response.Body, N: maxExportSize + 1}
	reader := csv.NewReader(limited)
	reader.Comma = ':'
	reader.FieldsPerRecord = -1
	result := make([]store.Proxy, 0)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			if limited.N == 0 {
				return nil, fmt.Errorf("Rayobyte CSV exceeds size limit")
			}
			return result, nil
		}
		if err != nil {
			return nil, fmt.Errorf("invalid Rayobyte CSV")
		}
		if len(row) < 4 {
			continue
		}
		port, err := strconv.Atoi(row[1])
		if err != nil || port < 1 || port > 65535 || row[0] == "" {
			continue
		}
		result = append(result, store.NewProxy(r.Name(), row[0], port, row[2], row[3]))
	}
}
