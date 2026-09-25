package route_test

import (
	"slices"
	"testing"

	"github.com/bigbag/proxy-manager/internal/config"
	"github.com/bigbag/proxy-manager/internal/route"
)

func TestMatchUsesFirstRouteAndGlob(t *testing.T) {
	routes := config.Routes{Routes: []config.Route{
		{Pattern: "*.example.com", Providers: []string{"webshare"}, Strategy: config.RoundRobin},
		{Pattern: "*", Providers: []string{"rayobyte"}, Strategy: config.PriorityFallback},
	}}
	providers, strategy := route.Match(routes, "api.example.com")
	if !slices.Equal(providers, []string{"webshare"}) || strategy != config.RoundRobin {
		t.Fatalf("first match = %v, %q", providers, strategy)
	}
	providers, strategy = route.Match(routes, "other.test")
	if !slices.Equal(providers, []string{"rayobyte"}) || strategy != config.PriorityFallback {
		t.Fatalf("fallback = %v, %q", providers, strategy)
	}
	providers, _ = route.Match(config.Routes{}, "other.test")
	if providers != nil {
		t.Fatalf("unmatched route = %v", providers)
	}
}

func TestMatchIsCaseSensitiveAndSupportsQuestionMark(t *testing.T) {
	routes := config.Routes{Routes: []config.Route{{Pattern: "api?.example.com", Providers: []string{"webshare"}, Strategy: config.RoundRobin}}}
	if got, _ := route.Match(routes, "api1.example.com"); !slices.Equal(got, []string{"webshare"}) {
		t.Fatalf("single-character pattern = %v", got)
	}
	if got, _ := route.Match(routes, "API1.example.com"); got != nil {
		t.Fatalf("case-sensitive pattern = %v", got)
	}
}
