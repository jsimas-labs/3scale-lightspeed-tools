package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeProm serves canned Prometheus API responses and records the queries it
// received, so the query builders can be asserted without a cluster.
type fakeProm struct {
	*httptest.Server
	lastQuery map[string]string
}

func newFakeProm(t *testing.T, handler func(path string, form map[string]string) (any, int)) *fakeProm {
	t.Helper()
	f := &fakeProm{lastQuery: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k, v := range r.Form {
			form[k] = v[0]
		}
		f.lastQuery[r.URL.Path] = form["query"]
		body, code := handler(r.URL.Path, form)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.Close)
	return f
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
	f := newFakeProm(t, func(string, map[string]string) (any, int) {
		calls++
		return vector(
			vecEntry(map[string]string{"__name__": "upstream_status"}, "12"),
			vecEntry(map[string]string{"__name__": "total_response_time_seconds_bucket"}, "40"),
		), http.StatusOK
	})
	p := newPromClient(f.URL, "", false)
	ctx := context.Background()
	catalog, err := p.metricCatalog(ctx)
	if err != nil {
		t.Fatalf("metricCatalog: %v", err)
	}
	if !catalog["upstream_status"] || catalog["threescale_backend_calls"] {
		t.Errorf("unexpected catalog: %v", catalog)
	}
	if _, err := p.metricCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("the catalog should be cached, Prometheus was queried %d times", calls)
	}
	if q := f.lastQuery["/api/v1/query"]; !strings.Contains(q, "__name__=~") {
		t.Errorf("the catalog probe should be a single __name__ query, got %q", q)
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
		switch {
		case strings.Contains(q, "__name__"):
			return vector(vecEntry(map[string]string{"__name__": "upstream_status"}, "1")), http.StatusOK
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
		case strings.Contains(q, "__name__"):
			return vector(
				vecEntry(map[string]string{"__name__": "upstream_status"}, "1"),
				vecEntry(map[string]string{"__name__": "total_response_time_seconds_bucket"}, "1"),
				vecEntry(map[string]string{"__name__": "upstream_response_time_seconds_bucket"}, "1"),
				vecEntry(map[string]string{"__name__": "threescale_backend_calls"}, "1"),
			), http.StatusOK
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
		q := form["query"]
		if strings.Contains(q, "__name__") {
			return vector(vecEntry(map[string]string{"__name__": "upstream_status"}, "1")), http.StatusOK
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
