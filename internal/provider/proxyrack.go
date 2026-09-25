package provider

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/bigbag/proxy-manager/internal/store"
)

type ProxyRack struct {
	Address, APILogin, APIKey string
	InitialPort, Slots        int
	Every                     time.Duration
}

func (p *ProxyRack) Name() string { return "proxyrack" }
func (p *ProxyRack) Interval() time.Duration {
	if p.Every > 0 {
		return p.Every
	}
	return time.Hour
}

func (p *ProxyRack) Fetch(ctx context.Context) ([]store.Proxy, error) {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(p.Address, strconv.Itoa(p.InitialPort)))
	if err != nil {
		return nil, fmt.Errorf("ProxyRack probe failed")
	}
	_ = conn.Close()
	proxies := make([]store.Proxy, 0, p.Slots)
	for port := p.InitialPort; port < p.InitialPort+p.Slots; port++ {
		proxies = append(proxies, store.NewProxy(p.Name(), p.Address, port, p.APILogin, p.APIKey))
	}
	return proxies, nil
}
