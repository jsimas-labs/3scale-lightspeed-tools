package main

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", time.Hour, false},
		{"15m", 15 * time.Minute, false},
		{"1h", time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"0h", 0, true},
		{"1y", 0, true},
		{"abc", 0, true},
		{"60d", 0, true}, // beyond the 30d cap
	}
	for _, c := range cases {
		got, canonical, err := parseWindow(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseWindow(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseWindow(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseWindow(%q) = %v, want %v", c.in, got, c.want)
		}
		if canonical == "" {
			t.Errorf("parseWindow(%q) returned an empty canonical window", c.in)
		}
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[string]string{
		"200": "2xx", "301": "3xx", "404": "4xx", "502": "5xx",
		"0": "other", "": "other", "unknown": "other",
	}
	for in, want := range cases {
		if got := statusClass(in); got != want {
			t.Errorf("statusClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewStatusBreakdown(t *testing.T) {
	samples := []promSample{
		{Labels: map[string]string{"status": "200"}, Value: 70},
		{Labels: map[string]string{"status": "200", "pod": "b"}, Value: 10},
		{Labels: map[string]string{"status": "502"}, Value: 15},
		{Labels: map[string]string{"status": "404"}, Value: 5},
	}
	sb := newStatusBreakdown(samples)
	if sb.Total != 100 {
		t.Fatalf("Total = %v, want 100", sb.Total)
	}
	if sb.ByClass["2xx"] != 80 {
		t.Errorf("2xx = %v, want 80 (samples with the same status must be merged)", sb.ByClass["2xx"])
	}
	if sb.ByClass["5xx"] != 15 || sb.ByClass["4xx"] != 5 {
		t.Errorf("class split wrong: %v", sb.ByClass)
	}
	if sb.ByStatus[0].Status != "200" {
		t.Errorf("breakdown must be sorted by count, got %v first", sb.ByStatus[0].Status)
	}
	if got := sb.pct(15); got != 15 {
		t.Errorf("pct(15) = %v, want 15", got)
	}
	if got := (statusBreakdown{}).pct(5); got != 0 {
		t.Errorf("pct on an empty breakdown must be 0, got %v", got)
	}
}

func TestMetricScopeMatchers(t *testing.T) {
	if got := (metricScope{}).matchers(); got != "" {
		t.Errorf("empty scope should produce no matchers, got %q", got)
	}
	got := metricScope{Namespaces: []string{"3scale", "team-a"}}.matchers()
	if !strings.Contains(got, `namespace=~"3scale|team-a"`) {
		t.Errorf("namespace matcher missing: %q", got)
	}
	got = metricScope{Gateway: "apicast-production"}.matchers()
	if !strings.Contains(got, `pod=~"apicast-production-.*"`) {
		t.Errorf("gateway matcher missing: %q", got)
	}
	got = metricScope{Namespaces: []string{"ns"}, Gateway: "gw"}.matchers()
	if strings.Count(got, ",") != 1 {
		t.Errorf("matchers must be comma separated: %q", got)
	}
}

func TestEscapeHelpers(t *testing.T) {
	if got := escapeLabelValue(`a"b\c`); got != `a\"b\\c` {
		t.Errorf("escapeLabelValue = %q", got)
	}
	if got := escapeRegex("api.v1"); got != `api\.v1` {
		t.Errorf("escapeRegex = %q", got)
	}
}

func TestDeploymentFromPod(t *testing.T) {
	cases := map[string]string{
		"apicast-production-7d9f8b6c5d-x2k9p": "apicast-production",
		"apicast-team-a-59d4c-abcde":          "apicast-team-a",
		"apicast":                             "apicast",
		"a-b":                                 "a-b",
	}
	for in, want := range cases {
		if got := deploymentFromPod(in); got != want {
			t.Errorf("deploymentFromPod(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvedAPISelector(t *testing.T) {
	if got := (resolvedAPI{ID: "7"}).selector(); got != `service_id="7"` {
		t.Errorf("selector by id = %q", got)
	}
	if got := (resolvedAPI{SystemName: "echo"}).selector(); got != `service_system_name="echo"` {
		t.Errorf("selector by system name = %q", got)
	}
	// The id is preferred because it is stable across renames.
	if got := (resolvedAPI{ID: "7", SystemName: "echo"}).selector(); got != `service_id="7"` {
		t.Errorf("selector should prefer the id, got %q", got)
	}
	if got := (resolvedAPI{}).selector(); got != "" {
		t.Errorf("empty identity must yield no selector, got %q", got)
	}
}

func TestMatchServices(t *testing.T) {
	svcs := []apiService{
		{ID: "2", Name: "Echo API", SystemName: "echo_api"},
		{ID: "3", Name: "Payments", SystemName: "payments"},
		{ID: "4", Name: "Payments Legacy", SystemName: "payments_legacy"},
	}
	if got := matchServices(svcs, "3"); len(got) != 1 || got[0].ID != "3" {
		t.Errorf("match by id failed: %v", got)
	}
	if got := matchServices(svcs, "echo api"); len(got) != 1 || got[0].ID != "2" {
		t.Errorf("match by display name failed: %v", got)
	}
	got := matchServices(svcs, "payments")
	if len(got) != 2 || got[0].ID != "3" {
		t.Errorf("exact match must rank before the prefix match: %v", got)
	}
	if got := matchServices(svcs, ""); got != nil {
		t.Errorf("empty query must match nothing, got %v", got)
	}
}

func TestApiFindingsFlagsUpstreamFailures(t *testing.T) {
	sb := newStatusBreakdown([]promSample{
		{Labels: map[string]string{"status": "200"}, Value: 50},
		{Labels: map[string]string{"status": "502"}, Value: 50},
	})
	fs := apiFindings(sb, latency{}, latency{}, map[string]queryResult{})
	joined := renderFindings(fs)
	if !strings.Contains(joined, "CRITICAL") {
		t.Errorf("50%% of 502 must raise a critical finding:\n%s", joined)
	}
	if !strings.Contains(joined, "502") {
		t.Errorf("the finding should name the status code:\n%s", joined)
	}
}

func TestApiFindingsHealthyTraffic(t *testing.T) {
	sb := newStatusBreakdown([]promSample{{Labels: map[string]string{"status": "200"}, Value: 1000}})
	fs := apiFindings(sb, latency{Available: true, P95: 0.1}, latency{Available: true, P95: 0.09}, map[string]queryResult{})
	if len(fs) != 1 || fs[0].Severity != "info" {
		t.Errorf("healthy traffic should produce a single info finding, got %v", fs)
	}
}

func TestApiFindingsDetectsGatewayOverhead(t *testing.T) {
	sb := newStatusBreakdown([]promSample{{Labels: map[string]string{"status": "200"}, Value: 100}})
	// Upstream answers in 200ms but the client waits 2s: the gap is APIcast's.
	fs := apiFindings(sb, latency{Available: true, P95: 2}, latency{Available: true, P95: 0.2}, map[string]queryResult{})
	if !strings.Contains(renderFindings(fs), "APIcast adds") {
		t.Errorf("expected a gateway-overhead finding, got %v", fs)
	}
}

func TestApiFindingsDetectsBackendAuthFailures(t *testing.T) {
	sb := newStatusBreakdown([]promSample{{Labels: map[string]string{"status": "200"}, Value: 100}})
	res := map[string]queryResult{"backend": {samples: []promSample{
		{Labels: map[string]string{"endpoint": "authrep", "status": "200"}, Value: 90},
		{Labels: map[string]string{"endpoint": "authrep", "status": "500"}, Value: 10},
	}}}
	if !strings.Contains(renderFindings(apiFindings(sb, latency{}, latency{}, res)), "3scale backend") {
		t.Error("failing backend calls must be reported")
	}
}

func TestApiFindingsNoTraffic(t *testing.T) {
	fs := apiFindings(statusBreakdown{ByClass: map[string]float64{}}, latency{}, latency{}, map[string]queryResult{})
	if len(fs) != 1 || fs[0].Severity != "warning" {
		t.Errorf("no traffic should warn, got %v", fs)
	}
}

func TestMetricNameResolution(t *testing.T) {
	catalog := map[string]bool{"upstream_status": true, "total_response_time_seconds_bucket": true}
	if got := metricName(catalog, "upstream_status"); got != "upstream_status" {
		t.Errorf("metricName = %q", got)
	}
	if got := metricName(catalog, "threescale_backend_calls"); got != "" {
		t.Errorf("absent metric must resolve to an empty name, got %q", got)
	}
	// The _total spelling is accepted when that is what the scraper produced.
	if got := metricName(map[string]bool{"upstream_status_total": true}, "upstream_status"); got != "upstream_status_total" {
		t.Errorf("metricName with the _total suffix = %q", got)
	}
}

func TestSampleValue(t *testing.T) {
	if v, ok := sampleValue([]any{1.0, "42.5"}); !ok || v != 42.5 {
		t.Errorf("sampleValue = %v, %v", v, ok)
	}
	if _, ok := sampleValue([]any{1.0, "NaN"}); ok {
		t.Error("NaN samples must be dropped")
	}
	if _, ok := sampleValue([]any{1.0}); ok {
		t.Error("malformed pairs must be rejected")
	}
}

func TestRedactEnvValueNeverLeaksTheAccessToken(t *testing.T) {
	// THREESCALE_PORTAL_ENDPOINT embeds the access token as the URL user info.
	got := redactEnvValue(envVar("THREESCALE_PORTAL_ENDPOINT", "https://abc123secret@system-master.3scale.svc:3000"))
	if strings.Contains(got, "abc123secret") {
		t.Fatalf("access token leaked: %q", got)
	}
	if !strings.Contains(got, "system-master") {
		t.Errorf("the host should survive redaction: %q", got)
	}
}

func TestDedupeDoesNotAliasInput(t *testing.T) {
	in := []string{"a", "a", "b"}
	out := dedupe(in)
	if len(out) != 2 || out[0] != "a" || out[1] != "b" {
		t.Fatalf("dedupe = %v", out)
	}
	if in[1] != "a" {
		t.Errorf("dedupe must not modify its input, got %v", in)
	}
}

func TestTrendRendering(t *testing.T) {
	now := time.Now()
	points := []trendPoint{
		{At: now.Add(-2 * time.Minute), Total: 100, C5xx: 0},
		{At: now.Add(-time.Minute), Total: 50, C4xx: 10, C5xx: 40},
	}
	out := renderTrend(points)
	if !strings.Contains(out, "REQUESTS") || !strings.Contains(out, "#") {
		t.Errorf("trend should render a header and bars:\n%s", out)
	}
	if renderTrend(nil) == "" {
		t.Error("an empty trend must still say something")
	}
}

// envVar is a test helper building a literal environment variable.
func envVar(name, value string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: value} }

func TestRedactURLCredentials(t *testing.T) {
	cases := map[string]string{
		"https://token123@system-master.3scale.svc:3000": "https://[credentials redacted]@system-master.3scale.svc:3000",
		"https://user:pw@example.com/path":               "https://[credentials redacted]@example.com/path",
		"https://example.com/no-credentials":             "https://example.com/no-credentials",
	}
	for in, want := range cases {
		if got := redactURLCredentials(in); got != want {
			t.Errorf("redactURLCredentials(%q) = %q, want %q", in, got, want)
		}
	}
	if got := redactURLCredentials("::not a url::"); !strings.Contains(got, "redacted") {
		t.Errorf("an unparseable value must still be redacted, got %q", got)
	}
}

func TestRedactEnvValueDescribesSecretRefs(t *testing.T) {
	e := corev1.EnvVar{
		Name: "THREESCALE_PORTAL_ENDPOINT",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "apicast-config"},
				Key:                  "password",
			},
		},
	}
	got := redactEnvValue(e)
	if !strings.Contains(got, "apicast-config") || !strings.Contains(got, "password") {
		t.Errorf("secret references should be described, got %q", got)
	}
}

func TestGatewayFlags(t *testing.T) {
	g := apicastGateway{Env: map[string]string{"APICAST_EXTENDED_METRICS": "TRUE"}}
	if !g.extendedMetrics() {
		t.Error("the extended-metrics flag must be case insensitive")
	}
	if g.responseCodes() {
		t.Error("an unset APICAST_RESPONSE_CODES must read as disabled")
	}
}

func TestRenderGatewaysWarnsAboutMissingExtendedMetrics(t *testing.T) {
	out := renderGateways([]apicastGateway{
		{Namespace: "team-a", Deployment: "apicast-team-a", Source: "APIcast operator (APIcast/team-a)", Ready: 1, Desired: 1, Env: map[string]string{}},
	}, nil)
	if !strings.Contains(out, "APICAST_EXTENDED_METRICS is not enabled") {
		t.Errorf("gateways without extended metrics must be called out:\n%s", out)
	}
	if !strings.Contains(out, "team-a") {
		t.Errorf("the namespace must appear in the report:\n%s", out)
	}
	if empty := renderGateways(nil, nil); !strings.Contains(empty, "No APIcast gateways found") {
		t.Errorf("empty discovery must explain itself:\n%s", empty)
	}
}

// Removing the namespace matcher must leave valid PromQL: a naive replacement
// produced "{,status=~\"5..\"}", which Prometheus rejects.
func TestStripMatcherLeavesValidSelectors(t *testing.T) {
	ns := `namespace=~"3scale"`
	cases := map[string]string{
		`sum(increase(upstream_status{namespace=~"3scale",status=~"5.."}[1h]))`: `sum(increase(upstream_status{status=~"5.."}[1h]))`,
		`sum(increase(upstream_status{status=~"5..",namespace=~"3scale"}[1h]))`: `sum(increase(upstream_status{status=~"5.."}[1h]))`,
		`sum(increase(upstream_status{namespace=~"3scale"}[1h]))`:               `sum(increase(upstream_status{}[1h]))`,
		`min by (dict) (a{namespace=~"3scale"} / b{namespace=~"3scale"})`:       `min by (dict) (a{} / b{})`,
	}
	for in, want := range cases {
		if got := stripMatcher(in, ns); got != want {
			t.Errorf("stripMatcher(%q)\n = %q\nwant %q", in, got, want)
		}
	}
	for _, got := range cases {
		if strings.Contains(got, "{,") || strings.Contains(got, ",}") || strings.Contains(got, ",,") {
			t.Errorf("dangling comma in %q", got)
		}
	}
	if got := stripMatcher(`up{job="x"}`, ""); got != `up{job="x"}` {
		t.Errorf("an empty matcher must be a no-op, got %q", got)
	}
}
