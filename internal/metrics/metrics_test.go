package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := New()
	routes := r.Counter("vecta_routes_total", "Players routed", "reason", "server")
	routes.Inc("cookie", "survival")
	routes.Inc("cookie", "survival")
	routes.Inc("subdomain", `we"ird`)
	r.GaugeFunc("vecta_servers_online", "Servers online", func() float64 { return 3 })
	var nilCounter *CounterVec
	nilCounter.Inc("ignored")

	var sb strings.Builder
	r.Write(&sb)
	out := sb.String()
	for _, want := range []string{
		"# TYPE vecta_routes_total counter",
		`vecta_routes_total{reason="cookie",server="survival"} 2`,
		`vecta_routes_total{reason="subdomain",server="we\"ird"} 1`,
		"# TYPE vecta_servers_online gauge",
		"vecta_servers_online 3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
