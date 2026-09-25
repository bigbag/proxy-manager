package route

import (
	"path"

	"github.com/bigbag/proxy-manager/internal/config"
)

func Match(routes config.Routes, host string) ([]string, config.Strategy) {
	for _, r := range routes.Routes {
		if ok, _ := path.Match(r.Pattern, host); ok {
			return r.Providers, r.Strategy
		}
	}
	return nil, config.RoundRobin
}
