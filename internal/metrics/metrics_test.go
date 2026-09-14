package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := New()
	routes := r.Counter("anymcp_routes_total", "Players routed", "reason", "server")
	routes.Inc("cookie", "survival")
	routes.Inc("cookie", "survival")
	routes.Inc("subdomain", `we"ird`)
	r.GaugeFunc("anymcp_servers_online", "Servers online", func() float64 { return 3 })
	var nilCounter *CounterVec
	nilCounter.Inc("ignored")

	var sb strings.Builder
	r.Write(&sb)
	out := sb.String()
	for _, want := range []string{
		"# TYPE anymcp_routes_total counter",
		`anymcp_routes_total{reason="cookie",server="survival"} 2`,
		`anymcp_routes_total{reason="subdomain",server="we\"ird"} 1`,
		"# TYPE anymcp_servers_online gauge",
		"anymcp_servers_online 3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
