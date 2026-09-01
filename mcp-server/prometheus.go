package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Prometheus/Thanos access.
//
// APIcast exposes Prometheus metrics on port 9421 (/metrics). On OpenShift
// those metrics are collected by *user workload monitoring* and are queryable
// through the Thanos querier in openshift-monitoring, which federates the
// platform and user-workload Prometheus instances. Port 9091 answers
// cross-namespace queries for callers holding the cluster-monitoring-view
// ClusterRole; that is what the MCP ServiceAccount is granted, so a single
// query can cover APIcast gateways spread across many namespaces.
//
// The client is intentionally dependency-free: it speaks the documented
// HTTP API (/api/v1/query, /api/v1/query_range, /api/v1/label/<l>/values).

const (
	defaultPrometheusURL = "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"
	saTokenFile          = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- path, not a credential
	serviceCAFile        = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"
	saCAFile             = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	maxPromSeries        = 500
	metricCatalogTTL     = 5 * time.Minute
)

// promClient queries a Prometheus-compatible HTTP API.
type promClient struct {
	baseURL string
	token   string // static token; when empty the ServiceAccount token file is read per request
	http    *http.Client

	mu         sync.Mutex
	catalog    map[string]bool
	catalogAt  time.Time
	catalogErr error
}

func newPromClient(baseURL, token string, insecure bool) *promClient {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12} // #nosec G402 -- InsecureSkipVerify is opt-in below
	if insecure {
		tlsCfg.InsecureSkipVerify = true
	} else if pool := caPool(); pool != nil {
		tlsCfg.RootCAs = pool
	}
	return &promClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}
}

// caPool returns the system pool extended with the OpenShift service CA so
// that the in-cluster thanos-querier certificate validates.
func caPool() *x509.CertPool {
	pool, sysErr := x509.SystemCertPool()
	if pool == nil {
		pool = x509.NewCertPool()
	}
	added := false
	for _, f := range []string{serviceCAFile, saCAFile} {
		if pem, err := os.ReadFile(f); err == nil && pool.AppendCertsFromPEM(pem) {
			added = true
		}
	}
	if sysErr != nil && !added {
		// An empty pool would reject every certificate; let the TLS stack use
		// its own defaults instead.
		return nil
	}
	return pool
}

// enabled reports whether metric-based tools can run at all.
func (p *promClient) enabled() bool { return p != nil && p.baseURL != "" }

const promDisabledMsg = "Metrics are unavailable: this MCP server was started without a Prometheus endpoint " +
	"(-prometheus-url / PROMETHEUS_URL is empty). Set it to the OpenShift Thanos querier " +
	"(https://thanos-querier.openshift-monitoring.svc.cluster.local:9091) or to any Prometheus that scrapes APIcast."

func (p *promClient) bearer() string {
	if p.token != "" {
		return p.token
	}
	// Projected ServiceAccount tokens rotate, so re-read on every request.
	if b, err := os.ReadFile(saTokenFile); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// promResponse is the envelope every Prometheus API endpoint returns. The
// shape of "data" differs per endpoint — an object for queries, a plain array
// for label values — so it is decoded by the caller.
type promResponse struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Warnings  []string        `json:"warnings"`
}

// queryResult decodes the {resultType, result} object returned by the query
// endpoints.
func (r *promResponse) queryResult() (json.RawMessage, error) {
	var data struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return nil, fmt.Errorf("decoding the Prometheus result envelope: %w", err)
	}
	return data.Result, nil
}

// do issues a request. Queries are sent as POST so long PromQL expressions are
// never truncated by URL length limits; the metadata endpoints (label values)
// accept GET only.
func (p *promClient) do(ctx context.Context, method, path string, params url.Values) (*promResponse, error) {
	if !p.enabled() {
		return nil, fmt.Errorf("%s", promDisabledMsg)
	}
	endpoint := p.baseURL + path
	var req *http.Request
	var err error
	if method == http.MethodGet {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if tok := p.bearer(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying Prometheus at %s: %w", p.baseURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading Prometheus response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("Prometheus returned %s: the MCP ServiceAccount needs the cluster-monitoring-view ClusterRole "+
			"(oc adm policy add-cluster-role-to-user cluster-monitoring-view -z <sa> -n <ns>)", resp.Status)
	}
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("Prometheus returned %s with a non-JSON body: %s", resp.Status, truncate(string(body), 300))
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("Prometheus error (%s): %s", pr.ErrorType, pr.Error)
	}
	return &pr, nil
}

// ---- instant queries ----

// promSample is one labelled value of an instant query result.
type promSample struct {
	Labels map[string]string
	Value  float64
}

func (s promSample) label(k string) string { return s.Labels[k] }

func (p *promClient) instant(ctx context.Context, query string, at time.Time) ([]promSample, error) {
	params := url.Values{"query": {query}}
	if !at.IsZero() {
		params.Set("time", strconv.FormatInt(at.Unix(), 10))
	}
	pr, err := p.do(ctx, http.MethodPost, "/api/v1/query", params)
	if err != nil {
		return nil, err
	}
	result, err := pr.queryResult()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("decoding instant query result: %w", err)
	}
	out := make([]promSample, 0, len(raw))
	for _, r := range raw {
		v, ok := sampleValue(r.Value)
		if !ok {
			continue
		}
		out = append(out, promSample{Labels: r.Metric, Value: v})
	}
	return out, nil
}

// promSeries is one labelled time series of a range query result.
type promSeries struct {
	Labels map[string]string
	Points []promPoint
}

type promPoint struct {
	At    time.Time
	Value float64
}

func (p *promClient) rangeQuery(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]promSeries, error) {
	params := url.Values{
		"query": {query},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.Itoa(int(step.Seconds())) + "s"},
	}
	pr, err := p.do(ctx, http.MethodPost, "/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}
	result, err := pr.queryResult()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Metric map[string]string `json:"metric"`
		Values [][]any           `json:"values"`
	}
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("decoding range query result: %w", err)
	}
	out := make([]promSeries, 0, len(raw))
	for _, r := range raw {
		s := promSeries{Labels: r.Metric}
		for _, v := range r.Values {
			val, ok := sampleValue(v)
			if !ok {
				continue
			}
			ts, _ := v[0].(float64)
			s.Points = append(s.Points, promPoint{At: time.Unix(int64(ts), 0), Value: val})
		}
		out = append(out, s)
	}
	return out, nil
}

// sampleValue decodes the [<timestamp>, "<value>"] pair Prometheus returns.
func sampleValue(v []any) (float64, bool) {
	if len(v) != 2 {
		return 0, false
	}
	s, ok := v[1].(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || isNaN(f) {
		return 0, false
	}
	return f, true
}

func isNaN(f float64) bool { return f != f }

// ---- metric catalog ----

// apicastMetricCandidates lists, per logical metric, the exposition names that
// may be present. APIcast exposes counters without the _total suffix, but
// scrapers using the OpenMetrics parser may normalise them, so both are tried.
var apicastMetricCandidates = map[string][]string{
	"upstream_status":              {"upstream_status", "upstream_status_total"},
	"total_response_time":          {"total_response_time_seconds_bucket"},
	"upstream_response_time":       {"upstream_response_time_seconds_bucket"},
	"threescale_backend_calls":     {"threescale_backend_calls", "threescale_backend_calls_total"},
	"nginx_http_connections":       {"nginx_http_connections"},
	"nginx_error_log":              {"nginx_error_log", "nginx_error_log_total"},
	"nginx_metric_errors":          {"nginx_metric_errors_total", "nginx_metric_errors"},
	"openresty_shdict_free_space":  {"openresty_shdict_free_space"},
	"openresty_shdict_capacity":    {"openresty_shdict_capacity"},
	"batching_policy_auths_hits":   {"batching_policy_auths_cache_hits", "batching_policy_auths_cache_hits_total"},
	"batching_policy_auths_misses": {"batching_policy_auths_cache_misses", "batching_policy_auths_cache_misses_total"},
	"total_response_time_sum":      {"total_response_time_seconds_sum"},
	"total_response_time_count":    {"total_response_time_seconds_count"},
	"upstream_response_time_sum":   {"upstream_response_time_seconds_sum"},
	"upstream_response_time_count": {"upstream_response_time_seconds_count"},
}

// catalogNames returns every exposition name the catalog probe asks about.
func catalogNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, cands := range apicastMetricCandidates {
		for _, c := range cands {
			if !seen[c] {
				seen[c] = true
				names = append(names, c)
			}
		}
	}
	sort.Strings(names)
	return names
}

// metricCatalog returns the set of APIcast metric names actually present in
// Prometheus. One instant query answers "are APIcast metrics being scraped at
// all?" and "which exposition naming is in use?".
func (p *promClient) metricCatalog(ctx context.Context) (map[string]bool, error) {
	p.mu.Lock()
	if p.catalog != nil && time.Since(p.catalogAt) < metricCatalogTTL {
		c, err := p.catalog, p.catalogErr
		p.mu.Unlock()
		return c, err
	}
	p.mu.Unlock()

	query := fmt.Sprintf("count by (__name__) ({__name__=~%q})", strings.Join(catalogNames(), "|"))
	samples, err := p.instant(ctx, query, time.Time{})
	catalog := map[string]bool{}
	for _, s := range samples {
		if n := s.label("__name__"); n != "" {
			catalog[n] = true
		}
	}

	p.mu.Lock()
	p.catalog, p.catalogAt, p.catalogErr = catalog, time.Now(), err
	p.mu.Unlock()
	return catalog, err
}

// metricName resolves a logical metric to the exposition name present in this
// cluster, or "" when the metric is not being scraped.
func metricName(catalog map[string]bool, logical string) string {
	for _, c := range apicastMetricCandidates[logical] {
		if catalog[c] {
			return c
		}
	}
	return ""
}

// ---- label discovery ----

func (p *promClient) labelValues(ctx context.Context, label string, matches []string, start, end time.Time) ([]string, error) {
	params := url.Values{}
	for _, m := range matches {
		params.Add("match[]", m)
	}
	if !start.IsZero() {
		params.Set("start", strconv.FormatInt(start.Unix(), 10))
	}
	if !end.IsZero() {
		params.Set("end", strconv.FormatInt(end.Unix(), 10))
	}
	// This endpoint returns "data" as a plain JSON array and accepts GET only.
	pr, err := p.do(ctx, http.MethodGet, "/api/v1/label/"+url.PathEscape(label)+"/values", params)
	if err != nil {
		return nil, err
	}
	var values []string
	if err := json.Unmarshal(pr.Data, &values); err != nil {
		return nil, fmt.Errorf("decoding label values: %w", err)
	}
	sort.Strings(values)
	return values, nil
}

// ---- helpers ----

var windowRe = regexp.MustCompile(`^([0-9]+)(s|m|h|d|w)$`)

// parseWindow validates a PromQL-style range such as "15m", "6h" or "7d" and
// returns both the duration and the canonical string to embed in queries.
func parseWindow(s string) (time.Duration, string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Hour, "1h", nil
	}
	m := windowRe.FindStringSubmatch(s)
	if m == nil {
		return 0, "", fmt.Errorf("invalid window %q: use a number followed by s, m, h, d or w (e.g. 15m, 1h, 24h, 7d)", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, "", fmt.Errorf("invalid window %q: the amount must be a positive integer", s)
	}
	unit := map[string]time.Duration{
		"s": time.Second, "m": time.Minute, "h": time.Hour,
		"d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
	}[m[2]]
	d := time.Duration(n) * unit
	if d > 30*24*time.Hour {
		return 0, "", fmt.Errorf("window %q is too long: 30d is the maximum", s)
	}
	return d, s, nil
}

// escapeLabelValue quotes a value for safe interpolation into a PromQL matcher.
func escapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// escapeRegex escapes a literal string for use inside a PromQL =~ matcher.
func escapeRegex(v string) string {
	return regexp.QuoteMeta(v)
}

// selectorSuffix appends extra matchers to a metric selector body.
func joinSelectors(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ",")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
