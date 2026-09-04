package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeProm serves canned Prometheus API responses and records the queries it
// received, so the query builders can be asserted without a cluster.
type fakeProm struct {
	*httptest.Server
	lastQuery map[string]string
	lastMatch map[string]string
}

func newFakeProm(t *testing.T, handler func(path string, form map[string]string) (any, int)) *fakeProm {
	t.Helper()
	f := &fakeProm{lastQuery: map[string]string{}, lastMatch: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k, v := range r.Form {
			form[k] = v[0]
		}
		f.lastQuery[r.URL.Path] = form["query"]
		if m := r.Form["match[]"]; len(m) > 0 {
			f.lastMatch[r.URL.Path] = m[0]
		}
		if m := r.Form["match[]"]; len(m) > 0 {
			form["match[]"] = m[0]
		}
		body, code := handler(r.URL.Path, form)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.Close)
	return f
}

// labelValues is the shape the /api/v1/label/<l>/values endpoint returns.
func labelValues(names ...string) map[string]any {
	return map[string]any{"status": "success", "data": names}
}

func vector(entries ...map[string]any) map[string]any {
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": entries}}
}

func vecEntry(labels map[string]string, value string) map[string]any {
	return map[string]any{"metric": labels, "value": []any{1700000000, value}}
}

func TestPromInstantParsesVector(t *testing.T) {
	f := newFakeProm(t, func(string, map[string]string) (any, int) {
		return vector(
			vecEntry(map[string]string{"status": "200"}, "120.5"),
			vecEntry(map[string]string{"status": "502"}, "3"),
		), http.StatusOK
	})
	p := newPromClient(f.URL, "tok", false)
	samples, err := p.instant(context.Background(), `sum by (status) (increase(upstream_status[1h]))`, time.Time{})
	if err != nil {
		t.Fatalf("instant: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	if samples[0].label("status") != "200" || samples[0].Value != 120.5 {
		t.Errorf("unexpected first sample: %+v", samples[0])
	}
}

func TestPromReportsQueryErrors(t *testing.T) {
	f := newFakeProm(t, func(string, map[string]string) (any, int) {
		return map[string]any{"status": "error", "errorType": "bad_data", "error": "parse error"}, http.StatusBadRequest
	})
	p := newPromClient(f.URL, "tok", false)
	_, err := p.instant(context.Background(), "boom(", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("expected the Prometheus error to surface, got %v", err)
	}
}

func TestPromExplainsMissingPermissions(t *testing.T) {
	f := newFakeProm(t, func(string, map[string]string) (any, int) {
		return map[string]any{"status": "error", "error": "forbidden"}, http.StatusForbidden
	})
	p := newPromClient(f.URL, "tok", false)
	_, err := p.instant(context.Background(), "up", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "cluster-monitoring-view") {
		t.Fatalf("a 403 should point at the missing ClusterRole, got %v", err)
	}
}

func TestPromRangeQuery(t *testing.T) {
	f := newFakeProm(t, func(string, map[string]string) (any, int) {
		return map[string]any{"status": "success", "data": map[string]any{
			"resultType": "matrix",
			"result": []any{map[string]any{
				"metric": map[string]string{"status": "500"},
				"values": []any{[]any{1700000000, "1"}, []any{1700000060, "2"}},
			}},
		}}, http.StatusOK
	})
	p := newPromClient(f.URL, "", false)
	end := time.Now()
	series, err := p.rangeQuery(context.Background(), "x", end.Add(-time.Hour), end, time.Minute)
	if err != nil {
		t.Fatalf("rangeQuery: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 2 || series[0].Points[1].Value != 2 {
		t.Fatalf("unexpected series: %+v", series)
	}
}

func TestMetricCatalogIsCachedAndDetectsNames(t *testing.T) {
	calls := 0
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		calls++
		return map[string]any{"status": "success",
			"data": []string{"upstream_status", "total_response_time_seconds_bucket"}}, http.StatusOK
	})
	p := newPromClient(f.URL, "", false)
	ctx := context.Background()
	catalog, err := p.metricCatalog(ctx, `namespace=~"3scale"`, "1h")
	if err != nil {
		t.Fatalf("metricCatalog: %v", err)
	}
	if !catalog["upstream_status"] || catalog["threescale_backend_calls"] {
		t.Errorf("unexpected catalog: %v", catalog)
	}
	if _, err := p.metricCatalog(ctx, `namespace=~"3scale"`, "1h"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("the catalog should be cached, Prometheus was queried %d times", calls)
	}
	m := f.lastMatch["/api/v1/label/__name__/values"]
	if !strings.Contains(m, "__name__=~") {
		t.Errorf("the catalog probe should select by __name__, got %q", m)
	}
	if !strings.Contains(m, `namespace=~"3scale"`) {
		t.Errorf("the catalog probe must be scoped to the namespaces, got %q", m)
	}
}

func TestPromDisabledWhenNoURL(t *testing.T) {
	p := newPromClient("", "", false)
	if p.enabled() {
		t.Fatal("an empty URL must disable the metric tools")
	}
	if _, err := p.instant(context.Background(), "up", time.Time{}); err == nil {
		t.Fatal("queries against a disabled client must fail loudly")
	}
}

func TestListAPIsBuildsPerServiceQueries(t *testing.T) {
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		q := form["query"]
		if strings.HasPrefix(path, "/api/v1/label/") {
			return labelValues("upstream_status"), http.StatusOK
		}
		switch {
		case strings.Contains(q, `status=~"5.."`):
			return vector(vecEntry(map[string]string{"service_id": "2", "service_system_name": "echo"}, "5")), http.StatusOK
		case strings.Contains(q, `status=~"4.."`):
			return vector(vecEntry(map[string]string{"service_id": "2", "service_system_name": "echo"}, "10")), http.StatusOK
		default:
			return vector(vecEntry(map[string]string{"service_id": "2", "service_system_name": "echo", "namespace": "team-a"}, "100")), http.StatusOK
		}
	})
	d := &deps{
		kc:    &k8sClients{defaultNamespace: "3scale"},
		prom:  newPromClient(f.URL, "", false),
		admin: newAdminClient(nil, "", "", false, true), // catalog disabled
	}
	out, err := listAPIs(context.Background(), d, listAPIsInput{Window: "1h"})
	if err != nil {
		t.Fatalf("listAPIs: %v", err)
	}
	for _, want := range []string{"echo", "team-a", "100", "15.0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
}

func TestQueryMetricsInstant(t *testing.T) {
	f := newFakeProm(t, func(string, map[string]string) (any, int) {
		return vector(vecEntry(map[string]string{"pod": "apicast-1"}, "7")), http.StatusOK
	})
	d := &deps{prom: newPromClient(f.URL, "", false)}
	out, err := queryMetrics(context.Background(), d, promQueryInput{Query: "up"})
	if err != nil {
		t.Fatalf("queryMetrics: %v", err)
	}
	if !strings.Contains(out, `pod="apicast-1"`) || !strings.Contains(out, "7") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if _, err := queryMetrics(context.Background(), d, promQueryInput{Query: "  "}); err == nil {
		t.Error("an empty query must be rejected")
	}
}

func TestLabelValuesUsesGetAndDecodesArray(t *testing.T) {
	var method, path string
	var matches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		matches = r.URL.Query()["match[]"]
		w.Header().Set("Content-Type", "application/json")
		// The label endpoint returns "data" as a plain array, unlike the
		// query endpoints which return {resultType, result}.
		_, _ = w.Write([]byte(`{"status":"success","data":["echo_api","payments"]}`))
	}))
	defer srv.Close()

	p := newPromClient(srv.URL, "", false)
	vals, err := p.labelValues(context.Background(), "service_system_name",
		[]string{"upstream_status"}, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("labelValues: %v", err)
	}
	if len(vals) != 2 || vals[0] != "echo_api" {
		t.Fatalf("unexpected values: %v", vals)
	}
	if method != http.MethodGet {
		t.Errorf("the label endpoint must be queried with GET, got %s", method)
	}
	if path != "/api/v1/label/service_system_name/values" {
		t.Errorf("unexpected path %q", path)
	}
	if len(matches) != 1 || matches[0] != "upstream_status" {
		t.Errorf("the match[] selector was not sent: %v", matches)
	}
}

// End-to-end shape of the main tool: a fake Prometheus feeds a 502 incident
// and the report must name the API, the status codes, the gateway and the cause.
func TestAnalyzeAPIProducesAFullReport(t *testing.T) {
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		q := form["query"]
		if strings.HasPrefix(path, "/api/v1/label/") {
			return labelValues("upstream_status", "total_response_time_seconds_bucket",
				"upstream_response_time_seconds_bucket", "threescale_backend_calls"), http.StatusOK
		}
		if path == "/api/v1/query_range" {
			return map[string]any{"status": "success", "data": map[string]any{
				"resultType": "matrix",
				"result": []any{map[string]any{
					"metric": map[string]string{"status": "502"},
					"values": []any{[]any{1700000000, "0.5"}, []any{1700000300, "1.5"}},
				}},
			}}, http.StatusOK
		}
		switch {
		case strings.Contains(q, "histogram_quantile(0.95") && strings.Contains(q, "total_response_time"):
			return vector(vecEntry(nil, "0.4")), http.StatusOK
		case strings.Contains(q, "histogram_quantile(0.95") && strings.Contains(q, "upstream_response_time"):
			return vector(vecEntry(nil, "0.05")), http.StatusOK
		case strings.Contains(q, "histogram_quantile"):
			return vector(vecEntry(nil, "0.6")), http.StatusOK
		case strings.Contains(q, "threescale_backend_calls"):
			return vector(vecEntry(map[string]string{"endpoint": "authrep", "status": "200"}, "1000")), http.StatusOK
		case strings.Contains(q, "sum by (service_id, service_system_name, namespace)"):
			return vector(vecEntry(map[string]string{"service_id": "7", "service_system_name": "echo_api", "namespace": "team-a"}, "1000")), http.StatusOK
		case strings.Contains(q, "sum by (namespace, pod)") && strings.Contains(q, `status=~"5.."`):
			return vector(vecEntry(map[string]string{"namespace": "team-a", "pod": "apicast-team-a-6d9f7b-abcde"}, "300")), http.StatusOK
		case strings.Contains(q, "sum by (namespace, pod)"):
			return vector(vecEntry(map[string]string{"namespace": "team-a", "pod": "apicast-team-a-6d9f7b-abcde"}, "1000")), http.StatusOK
		case strings.Contains(q, "sum by (status)"):
			return vector(
				vecEntry(map[string]string{"status": "200"}, "700"),
				vecEntry(map[string]string{"status": "502"}, "300"),
			), http.StatusOK
		}
		return vector(), http.StatusOK
	})

	d := &deps{
		kc:    &k8sClients{defaultNamespace: "3scale"},
		prom:  newPromClient(f.URL, "", false),
		admin: newAdminClient(nil, "", "", false, true),
	}
	out, err := analyzeAPI(context.Background(), d, analyzeAPIInput{API: "echo_api", Window: "1h"})
	if err != nil {
		t.Fatalf("analyzeAPI: %v", err)
	}
	for _, want := range []string{
		"echo_api",              // the resolved API
		"id=7",                  // resolved by label
		"502",                   // the failing status code
		"bad gateway",           // what that code means in APIcast
		"team-a/apicast-team-a", // the gateway, with the pod hash stripped
		"30.0%",                 // the 5xx share
		"authrep",               // backend calls section
		"CRITICAL",              // the finding severity
		"Private Base URL",      // the 502 remediation hint
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report should mention %q:\n%s", want, out)
		}
	}
	// Latency: total p95 400ms against upstream p95 50ms is gateway overhead.
	if !strings.Contains(out, "APIcast overhead") {
		t.Errorf("the latency table should isolate the gateway overhead:\n%s", out)
	}
}

func TestAnalyzeAPIUnknownAPIExplainsWhatIsAvailable(t *testing.T) {
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		if strings.HasPrefix(path, "/api/v1/label/") {
			return labelValues("upstream_status"), http.StatusOK
		}
		return vector(vecEntry(map[string]string{"service_id": "7", "service_system_name": "echo_api"}, "10")), http.StatusOK
	})
	d := &deps{
		kc:    &k8sClients{defaultNamespace: "3scale"},
		prom:  newPromClient(f.URL, "", false),
		admin: newAdminClient(nil, "", "", false, true),
	}
	_, err := analyzeAPI(context.Background(), d, analyzeAPIInput{API: "nonexistent"})
	if err == nil {
		t.Fatal("an unknown API must be an error, not an empty report")
	}
	if !strings.Contains(err.Error(), "echo_api") {
		t.Errorf("the error should list the APIs that do exist: %v", err)
	}
}

// A gateway that served traffic earlier in the window but nothing in the last
// five minutes must still be found: the old instant-query probe reported "no
// metrics" in that very common case.
func TestCatalogProbeSpansTheWindowNotTheLastFiveMinutes(t *testing.T) {
	var start, end string
	f := newFakeProm(t, func(_ string, form map[string]string) (any, int) {
		start, end = form["start"], form["end"]
		return map[string]any{"status": "success", "data": []string{"upstream_status"}}, http.StatusOK
	})
	p := newPromClient(f.URL, "", false)
	catalog, err := p.metricCatalog(context.Background(), `namespace=~"3scale"`, "24h")
	if err != nil {
		t.Fatalf("metricCatalog: %v", err)
	}
	if !catalog["upstream_status"] {
		t.Fatal("the metric was not found")
	}
	s0, e0 := mustAtoi(t, start), mustAtoi(t, end)
	if span := e0 - s0; span < 23*3600 || span > 25*3600 {
		t.Errorf("the probe must span the 24h window, got %ds", span)
	}
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", s, err)
	}
	return v
}

// A multi-metric selector must never be wrapped in a range function: those
// drop __name__, so series differing only by metric name collide and
// Prometheus rejects the query with "vector cannot contain metrics with the
// same labelset".
func TestCatalogProbeNeverUsesARangeFunction(t *testing.T) {
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		if path == "/api/v1/query" {
			t.Errorf("the catalog probe must not run an instant query: %q", form["query"])
		}
		return map[string]any{"status": "success", "data": []string{"upstream_status"}}, http.StatusOK
	})
	p := newPromClient(f.URL, "", false)
	if _, err := p.metricCatalog(context.Background(), "", "1h"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"count_over_time", "increase(", "rate("} {
		if strings.Contains(f.lastMatch["/api/v1/label/__name__/values"], bad) {
			t.Errorf("the probe must not use %s", bad)
		}
	}
}

// An empty catalog must not be cached: the operator fixes the pipeline and
// retries immediately, and a poisoned cache would keep saying "no metrics".
func TestEmptyCatalogIsNotCached(t *testing.T) {
	calls := 0
	f := newFakeProm(t, func(string, map[string]string) (any, int) {
		calls++
		if calls == 1 {
			return map[string]any{"status": "success", "data": []string{}}, http.StatusOK
		}
		return map[string]any{"status": "success", "data": []string{"upstream_status"}}, http.StatusOK
	})
	p := newPromClient(f.URL, "", false)
	ctx := context.Background()
	if c, _ := p.metricCatalog(ctx, "", "1h"); len(c) != 0 {
		t.Fatal("expected an empty first catalog")
	}
	c, err := p.metricCatalog(ctx, "", "1h")
	if err != nil {
		t.Fatal(err)
	}
	if !c["upstream_status"] {
		t.Errorf("the second call must re-probe rather than serve an empty cache (calls=%d)", calls)
	}
}

// Errors must not be cached either.
func TestCatalogErrorIsNotCached(t *testing.T) {
	calls := 0
	f := newFakeProm(t, func(string, map[string]string) (any, int) {
		calls++
		if calls == 1 {
			return map[string]any{"status": "error", "error": "transient"}, http.StatusServiceUnavailable
		}
		return map[string]any{"status": "success", "data": []string{"upstream_status"}}, http.StatusOK
	})
	p := newPromClient(f.URL, "", false)
	ctx := context.Background()
	if _, err := p.metricCatalog(ctx, "", "1h"); err == nil {
		t.Fatal("expected the first probe to fail")
	}
	if c, err := p.metricCatalog(ctx, "", "1h"); err != nil || !c["upstream_status"] {
		t.Errorf("a transient failure must not poison the cache: %v %v", c, err)
	}
}

// When the expected metric is absent, the tool must report what IS there
// rather than dead-ending.
func TestExplainNoMetricsReportsWhatExists(t *testing.T) {
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		if path == "/api/v1/label/__name__/values" {
			m := form["match[]"]
			if strings.Contains(m, "__name__=~") {
				return labelValues(), http.StatusOK // none of the expected names
			}
			return labelValues("nginx_http_connections", "container_cpu_usage_seconds_total"), http.StatusOK
		}
		if strings.HasPrefix(path, "/api/v1/label/") {
			return labelValues(), http.StatusOK
		}
		return vector(), http.StatusOK
	})
	d := &deps{
		kc:   &k8sClients{clientset: nil, defaultNamespace: "3scale"},
		prom: newPromClient(f.URL, "", false),
	}
	sc := metricScope{Namespaces: []string{"3scale"}, Origin: "THREESCALE_NAMESPACE=3scale"}
	out := explainNoMetrics(context.Background(), d, sc, "1h")

	if !strings.Contains(out, "THREESCALE_NAMESPACE=3scale") {
		t.Errorf("the scope actually queried must be reported:\n%s", out)
	}
	if !strings.Contains(out, "nginx_http_connections") {
		t.Errorf("gateway metrics that do exist must be listed:\n%s", out)
	}
	if strings.Contains(out, "container_cpu_usage_seconds_total") {
		t.Errorf("unrelated kubelet metrics must be filtered out:\n%s", out)
	}
}

func TestLooksLikeAPIcastMetric(t *testing.T) {
	for _, in := range []string{"upstream_status", "total_response_time_seconds_bucket", "threescale_backend_calls", "nginx_error_log", "openresty_shdict_capacity"} {
		if !looksLikeAPIcastMetric(in) {
			t.Errorf("%q should be recognised as a gateway metric", in)
		}
	}
	for _, in := range []string{"container_memory_usage_bytes", "kube_pod_info", "up"} {
		if looksLikeAPIcastMetric(in) {
			t.Errorf("%q should not be recognised as a gateway metric", in)
		}
	}
}

// When traffic exists but carries no service labels, the error must say which
// labels the metric really has — that is what distinguishes "extended metrics
// off" from "this build labels services differently".
func TestNoAPIMatchReportsActualLabelNames(t *testing.T) {
	err := noAPIMatchError("payments",
		[]metricAPI{{Requests: 500}}, // traffic, but no id/system name
		nil, nil, nil,
		[]string{"container", "endpoint", "namespace", "pod", "status"})
	msg := err.Error()
	if !strings.Contains(msg, "APICAST_EXTENDED_METRICS") {
		t.Errorf("the likely cause must be named:\n%s", msg)
	}
	if !strings.Contains(msg, "status") || !strings.Contains(msg, "pod") {
		t.Errorf("the labels actually present must be listed:\n%s", msg)
	}
}

func TestLabelNamesEndpoint(t *testing.T) {
	var path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":["status","namespace","service_id"]}`))
	}))
	defer srv.Close()
	p := newPromClient(srv.URL, "", false)
	names, err := p.labelNames(context.Background(), []string{"upstream_status"}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("labelNames: %v", err)
	}
	if len(names) != 3 || names[0] != "namespace" {
		t.Errorf("names = %v (must be sorted)", names)
	}
	if path != "/api/v1/labels" || method != http.MethodGet {
		t.Errorf("unexpected request %s %s", method, path)
	}
}

// Regression: the catalog probe once wrapped a multi-metric selector in
// count_over_time, which drops __name__ and made Prometheus reject the query
// with "vector cannot contain metrics with the same labelset" — breaking every
// metric tool. Reproduce the server behaviour and assert we no longer trip it.
func TestCatalogProbeAvoidsDuplicateLabelsetError(t *testing.T) {
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		if path == "/api/v1/query" {
			q := form["query"]
			// Emulate Prometheus: a range function over a multi-metric
			// selector collapses openresty_shdict_capacity and
			// _free_space onto the same labelset.
			for _, fn := range []string{"count_over_time", "increase(", "rate("} {
				if strings.Contains(q, fn) && strings.Contains(q, "__name__=~") {
					return map[string]any{"status": "error", "errorType": "execution",
						"error": "vector cannot contain metrics with the same labelset"}, http.StatusUnprocessableEntity
				}
			}
			return vector(), http.StatusOK
		}
		return labelValues("upstream_status", "openresty_shdict_capacity", "openresty_shdict_free_space"), http.StatusOK
	})
	p := newPromClient(f.URL, "", false)
	catalog, err := p.metricCatalog(context.Background(), `namespace=~"3scale"`, "1h")
	if err != nil {
		t.Fatalf("the catalog probe must not trip the duplicate-labelset error: %v", err)
	}
	if !catalog["upstream_status"] {
		t.Errorf("catalog = %v", catalog)
	}
}

// An API served from a namespace other than THREESCALE_NAMESPACE must still be
// found: the lookup is cluster-wide, and the analysis then narrows to where the
// traffic actually is.
func TestAnalyzeAPIFindsAPIOutsideTheConfiguredNamespace(t *testing.T) {
	var sawNamespaceFilterDuringLookup bool
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		q := form["query"]
		if strings.HasPrefix(path, "/api/v1/label/") {
			return labelValues("upstream_status"), http.StatusOK
		}
		if path == "/api/v1/query_range" {
			return map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}}, http.StatusOK
		}
		if strings.Contains(q, "sum by (service_id, service_system_name, namespace)") {
			if strings.Contains(q, "namespace=~") {
				sawNamespaceFilterDuringLookup = true
			}
			return vector(vecEntry(map[string]string{
				"service_id": "1314", "service_system_name": "payments", "namespace": "gateways-prod",
			}, "5000")), http.StatusOK
		}
		if strings.Contains(q, "sum by (status)") {
			return vector(vecEntry(map[string]string{"status": "200"}, "5000")), http.StatusOK
		}
		return vector(), http.StatusOK
	})
	d := &deps{
		// The API Manager is in "my-3scale"; the gateway serving this API is not.
		kc:    &k8sClients{defaultNamespace: "my-3scale"},
		prom:  newPromClient(f.URL, "", false),
		admin: newAdminClient(nil, "", "", false, true),
	}
	out, err := analyzeAPI(context.Background(), d, analyzeAPIInput{API: "1314", Window: "1h"})
	if err != nil {
		t.Fatalf("an API outside THREESCALE_NAMESPACE must still be found: %v", err)
	}
	if sawNamespaceFilterDuringLookup {
		t.Error("the API lookup must not be filtered by namespace")
	}
	if !strings.Contains(out, "gateways-prod") {
		t.Errorf("the report must say where the API is served from:\n%s", out)
	}
	if !strings.Contains(out, "1314") {
		t.Errorf("the API must be identified by its service id:\n%s", out)
	}
}

// In the usual topology the APIManager and the gateways are in different
// namespaces, so naming the APIManager namespace must NOT produce an empty
// report: the tool reports where the API actually is and says it did so.
func TestAnalyzeAPIWidensWhenNamespaceExcludesTheAPI(t *testing.T) {
	f := newFakeProm(t, func(path string, form map[string]string) (any, int) {
		q := form["query"]
		if strings.HasPrefix(path, "/api/v1/label/") {
			return labelValues("upstream_status"), http.StatusOK
		}
		if path == "/api/v1/query_range" {
			return map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}}, http.StatusOK
		}
		if strings.Contains(q, "sum by (service_id, service_system_name, namespace)") {
			return vector(vecEntry(map[string]string{
				"service_id": "1314", "service_system_name": "payments", "namespace": "gateways-prod",
			}, "5000")), http.StatusOK
		}
		if strings.Contains(q, "sum by (status)") {
			// Only answered for the gateway namespace: a query still pinned to
			// my-3scale would come back empty and fail the assertions below.
			if strings.Contains(q, "gateways-prod") {
				return vector(vecEntry(map[string]string{"status": "200"}, "5000")), http.StatusOK
			}
			return vector(), http.StatusOK
		}
		return vector(), http.StatusOK
	})
	d := &deps{
		kc:    &k8sClients{defaultNamespace: "my-3scale"},
		prom:  newPromClient(f.URL, "", false),
		admin: newAdminClient(nil, "", "", false, true),
	}
	out, err := analyzeAPI(context.Background(), d, analyzeAPIInput{API: "1314", Namespace: "my-3scale", Window: "1h"})
	if err != nil {
		t.Fatalf("analyzeAPI: %v", err)
	}
	if !strings.Contains(out, "gateways-prod") {
		t.Errorf("the report must name where the API is served from:\n%s", out)
	}
	if !strings.Contains(out, "You asked for namespace") {
		t.Errorf("overriding the namespace argument must be stated:\n%s", out)
	}
	if !strings.Contains(out, "total requests: 5000") {
		t.Errorf("the report must contain real figures, not zeros:\n%s", out)
	}
}
