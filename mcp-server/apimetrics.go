package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Per-API analysis from APIcast's Prometheus metrics.
//
// APIcast labels its traffic metrics with the HTTP status returned to the
// client (upstream_status) and, when APICAST_EXTENDED_METRICS=true, with
// service_id and service_system_name. Those two labels are what make it
// possible to answer "how is API X doing" rather than "how is the gateway
// doing". Latency comes from two histograms — upstream_response_time_seconds
// (time spent in the backend API) and total_response_time_seconds (that plus
// everything APIcast does: authrep against 3scale backend, policies, TLS) —
// and the gap between them isolates gateway overhead from upstream slowness.

// deps carries everything the tools need: cluster access, metrics and the
// optional 3scale product catalog.
type deps struct {
	kc    *k8sClients
	prom  *promClient
	admin *adminClient
}

// ---- scope ----

// metricScope narrows queries to a set of namespaces and/or one gateway.
type metricScope struct {
	Namespaces []string
	Gateway    string // Deployment name, matched against the pod label
	Origin     string // how Namespaces was decided, reported in every result
}

// matchers renders the scope as PromQL label matchers (without braces).
func (s metricScope) matchers() string {
	var parts []string
	if len(s.Namespaces) > 0 {
		alts := make([]string, 0, len(s.Namespaces))
		for _, ns := range s.Namespaces {
			alts = append(alts, escapeRegex(ns))
		}
		parts = append(parts, fmt.Sprintf(`namespace=~"%s"`, escapeLabelValue(strings.Join(alts, "|"))))
	}
	if s.Gateway != "" {
		parts = append(parts, fmt.Sprintf(`pod=~"%s-.*"`, escapeLabelValue(escapeRegex(s.Gateway))))
	}
	return joinSelectors(parts...)
}

func (s metricScope) describe() string {
	var where string
	switch {
	case len(s.Namespaces) == 0:
		where = "all namespaces of the cluster"
	default:
		where = "namespace(s) " + strings.Join(s.Namespaces, ", ")
	}
	if s.Gateway != "" {
		where = "gateway " + s.Gateway + " in " + where
	}
	if s.Origin != "" {
		where += " [" + s.Origin + "]"
	}
	return where
}

// resolveScope decides which namespaces a metric query covers.
//
// The default is the whole cluster. APIs are global objects in 3scale: a
// product is served by whichever gateways are configured for it, in whatever
// namespaces those live, so narrowing metric queries by namespace would hide
// APIs rather than help find them. An explicit "namespace" argument narrows
// deliberately.
//
// THREESCALE_NAMESPACE is NOT a metric filter. It identifies the API Manager
// installation, and is used to reach the Admin Portal for product names and by
// the installation tools; every metric report states both scopes so neither is
// silently ignored.
func resolveScope(ctx context.Context, d *deps, namespace, gateway string) metricScope {
	if namespace != "" {
		return metricScope{Namespaces: []string{namespace}, Gateway: gateway, Origin: "namespace argument"}
	}
	return metricScope{Gateway: gateway, Origin: "cluster-wide: APIs are not confined to one namespace"}
}

// clusterScope is the unrestricted scope used to find an API wherever it is.
func clusterScope(gateway string) metricScope {
	return metricScope{Gateway: gateway, Origin: "cluster-wide lookup"}
}

// ---- parallel query helper ----

type queryResult struct {
	samples []promSample
	err     error
}

func runQueries(ctx context.Context, p *promClient, queries map[string]string) map[string]queryResult {
	out := make(map[string]queryResult, len(queries))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, q := range queries {
		wg.Add(1)
		go func(name, q string) {
			defer wg.Done()
			s, err := p.instant(ctx, q, time.Time{})
			mu.Lock()
			out[name] = queryResult{samples: s, err: err}
			mu.Unlock()
		}(name, q)
	}
	wg.Wait()
	return out
}

// ---- API identity ----

// metricAPI is an API as Prometheus sees it.
type metricAPI struct {
	ID         string
	SystemName string
	Namespaces map[string]bool
	Requests   float64
}

func (m metricAPI) key() string { return m.ID + "\x00" + m.SystemName }

func (m metricAPI) label() string {
	switch {
	case m.ID != "" && m.SystemName != "":
		return fmt.Sprintf("%s (id %s)", m.SystemName, m.ID)
	case m.SystemName != "":
		return m.SystemName
	case m.ID != "":
		return "id " + m.ID
	}
	return "<unlabelled traffic>"
}

// apisFromMetrics lists the APIs that produced traffic in the window.
func apisFromMetrics(ctx context.Context, d *deps, sc metricScope, window string) ([]metricAPI, error) {
	catalog, err := d.prom.metricCatalog(ctx, sc.matchers(), window)
	if err != nil {
		return nil, err
	}
	name := metricName(catalog, "upstream_status")
	if name == "" {
		return nil, fmt.Errorf("%s", explainNoMetrics(ctx, d, sc, window))
	}
	q := fmt.Sprintf("sum by (service_id, service_system_name, namespace) (increase(%s{%s}[%s]))",
		name, sc.matchers(), window)
	samples, err := d.prom.instant(ctx, q, time.Time{})
	if err != nil {
		return nil, err
	}
	byKey := map[string]*metricAPI{}
	for _, s := range samples {
		m := metricAPI{ID: s.label("service_id"), SystemName: s.label("service_system_name")}
		cur, ok := byKey[m.key()]
		if !ok {
			m.Namespaces = map[string]bool{}
			cur = &m
			byKey[m.key()] = cur
		}
		cur.Requests += s.Value
		if ns := s.label("namespace"); ns != "" {
			cur.Namespaces[ns] = true
		}
	}
	out := make([]metricAPI, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out, nil
}

// resolvedAPI is the outcome of turning a free-form name/ID into the label
// selector used by every subsequent query.
type resolvedAPI struct {
	ID          string
	SystemName  string
	DisplayName string
	MatchedBy   string
	Namespaces  []string
	HasTraffic  bool
	CatalogNote string
}

// selector renders the PromQL matchers identifying this API.
func (r resolvedAPI) selector() string {
	switch {
	case r.ID != "":
		return fmt.Sprintf(`service_id="%s"`, escapeLabelValue(r.ID))
	case r.SystemName != "":
		return fmt.Sprintf(`service_system_name="%s"`, escapeLabelValue(r.SystemName))
	}
	return ""
}

func (r resolvedAPI) title() string {
	name := r.DisplayName
	if name == "" {
		name = r.SystemName
	}
	if name == "" {
		name = "id " + r.ID
	}
	var extra []string
	if r.SystemName != "" && r.SystemName != name {
		extra = append(extra, "system_name="+r.SystemName)
	}
	if r.ID != "" {
		extra = append(extra, "id="+r.ID)
	}
	if len(extra) > 0 {
		return fmt.Sprintf("%s (%s)", name, strings.Join(extra, ", "))
	}
	return name
}

// ambiguousError lists the candidates when a query matches more than one API.
type ambiguousError struct {
	query      string
	candidates []string
}

func (e *ambiguousError) Error() string {
	return fmt.Sprintf("%q matches %d APIs: %s. Re-run with the exact system name or the numeric service id.",
		e.query, len(e.candidates), strings.Join(e.candidates, "; "))
}

// resolveAPI turns a user-supplied name or ID into a concrete API. It matches
// against the Prometheus labels first (that is what queries need) and enriches
// with the Admin Portal catalog, which is the only place display names exist.
func resolveAPI(ctx context.Context, d *deps, query, namespace, window string, sc metricScope) (*resolvedAPI, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("'api' is required: pass the product name, its system name or its numeric service id")
	}

	// The identity lookup is ALWAYS cluster-wide: an API is a 3scale product,
	// not a namespaced object, and it is served by whichever gateways are
	// configured for it. Narrowing this by namespace would report "no such
	// API" for an API that plainly exists.
	metricAPIs, metricErr := apisFromMetrics(ctx, d, clusterScope(sc.Gateway), window)
	catalog, catalogEndpoint, catalogErr := d.admin.services(ctx, adminNamespace(ctx, d, namespace))

	catalogNote := ""
	switch {
	case catalogErr != nil:
		catalogNote = "3scale product catalog unavailable (" + firstLine(catalogErr.Error()) + "); matching against Prometheus labels only, so display names cannot be used."
	case catalogEndpoint != "":
		catalogNote = fmt.Sprintf("3scale product catalog read from %s (%d products).", catalogEndpoint, len(catalog))
	}

	byID := map[string]apiService{}
	bySys := map[string]apiService{}
	for _, s := range catalog {
		byID[s.ID] = s
		bySys[s.SystemName] = s
	}

	// 1. Candidates coming from metrics (these are guaranteed queryable).
	q := strings.ToLower(query)
	var hits []resolvedAPI
	for _, m := range metricAPIs {
		if m.ID == "" && m.SystemName == "" {
			continue
		}
		display := ""
		if s, ok := byID[m.ID]; ok {
			display = s.Name
		} else if s, ok := bySys[m.SystemName]; ok {
			display = s.Name
		}
		matched, how := matchAPIIdentity(q, m.ID, m.SystemName, display)
		if !matched {
			continue
		}
		hits = append(hits, resolvedAPI{
			ID: m.ID, SystemName: m.SystemName, DisplayName: display,
			MatchedBy: how + " (cluster-wide lookup)", Namespaces: keysOf(m.Namespaces), HasTraffic: true,
		})
	}

	// 2. Fall back to the catalog for APIs with no traffic in the window.
	if len(hits) == 0 && len(catalog) > 0 {
		for _, s := range matchServices(catalog, query) {
			hits = append(hits, resolvedAPI{
				ID: s.ID, SystemName: s.SystemName, DisplayName: s.Name,
				MatchedBy: "3scale product catalog", HasTraffic: false,
			})
		}
	}

	if len(hits) == 0 {
		// Only worth the extra round-trip on the failure path.
		var labelNames []string
		if mcat, cerr := d.prom.metricCatalog(ctx, sc.matchers(), window); cerr == nil {
			if m := metricName(mcat, "upstream_status"); m != "" {
				labelNames, _ = d.prom.labelNames(ctx, []string{m}, time.Now().Add(-24*time.Hour), time.Now())
			}
		}
		return nil, noAPIMatchError(query, metricAPIs, metricErr, catalog, catalogErr, labelNames)
	}
	// Prefer exact matches when several candidates survive.
	var exact []resolvedAPI
	for _, h := range hits {
		if strings.EqualFold(h.ID, query) || strings.EqualFold(h.SystemName, query) || strings.EqualFold(h.DisplayName, query) {
			exact = append(exact, h)
		}
	}
	if len(exact) > 0 {
		hits = exact
	}
	if len(hits) > 1 {
		var cands []string
		for _, h := range hits {
			cands = append(cands, h.title())
		}
		return nil, &ambiguousError{query: query, candidates: cands}
	}
	hits[0].CatalogNote = catalogNote
	return &hits[0], nil
}

func matchAPIIdentity(lowerQuery, id, sys, display string) (bool, string) {
	switch {
	case id != "" && strings.EqualFold(id, lowerQuery):
		return true, "service id"
	case sys != "" && strings.EqualFold(sys, lowerQuery):
		return true, "service system name"
	case display != "" && strings.EqualFold(display, lowerQuery):
		return true, "product name"
	case sys != "" && strings.Contains(strings.ToLower(sys), lowerQuery):
		return true, "partial system name"
	case display != "" && strings.Contains(strings.ToLower(display), lowerQuery):
		return true, "partial product name"
	}
	return false, ""
}

func noAPIMatchError(query string, metricAPIs []metricAPI, metricErr error, catalog []apiService, catalogErr error, labelNames []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "no API matching %q was found.\n", query)
	if metricErr != nil {
		fmt.Fprintf(&b, "Metric lookup failed: %v\n", metricErr)
	}
	labelled := 0
	for _, m := range metricAPIs {
		if m.ID != "" || m.SystemName != "" {
			labelled++
		}
	}
	switch {
	case len(metricAPIs) > 0 && labelled == 0:
		b.WriteString("APIcast traffic metrics exist but carry no service_id/service_system_name labels.\n")
		if len(labelNames) > 0 {
			fmt.Fprintf(&b, "Labels actually present on the traffic metric: %s\n", strings.Join(labelNames, ", "))
		}
		b.WriteString("The usual cause is APICAST_EXTENDED_METRICS not being \"true\" on the gateways — set it (APIcast CR spec, " +
			"or the APIManager apicast staging/production spec) and per-API analysis becomes available. " +
			"Run 3scale_list_apicast_gateways to see which gateways are affected.\n")
	case labelled > 0:
		b.WriteString("APIs currently visible in metrics: ")
		var names []string
		for i, m := range metricAPIs {
			if i == 15 {
				names = append(names, "…")
				break
			}
			names = append(names, m.label())
		}
		b.WriteString(strings.Join(names, ", ") + "\n")
	default:
		b.WriteString("No APIcast traffic was recorded in this window at all. Widen the window, check the namespace, " +
			"or run check_metrics_pipeline to verify that the gateways are being scraped.\n")
	}
	if catalogErr == nil && len(catalog) > 0 {
		var names []string
		for i, s := range catalog {
			if i == 15 {
				names = append(names, "…")
				break
			}
			names = append(names, fmt.Sprintf("%s (%s, id %s)", s.Name, s.SystemName, s.ID))
		}
		b.WriteString("Products configured in 3scale: " + strings.Join(names, ", ") + "\n")
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

// adminNamespace picks the namespace whose admin portal should answer catalog
// lookups: the requested one, else the configured default, else a discovered
// APIManager namespace.
func adminNamespace(ctx context.Context, d *deps, namespace string) string {
	if namespace != "" {
		return namespace
	}
	if d.admin == nil || d.admin.disabled {
		// No catalog lookup will happen; skip the cluster-wide search.
		return d.kc.defaultNamespace
	}
	if nss := discover3scaleNamespaces(ctx, d.kc); len(nss) > 0 {
		return nss[0]
	}
	return d.kc.defaultNamespace
}

// ---- status code accounting ----

type statusCount struct {
	Status string
	Count  float64
}

type statusBreakdown struct {
	Total    float64
	ByStatus []statusCount
	ByClass  map[string]float64 // "2xx".."5xx", plus "other"
}

func newStatusBreakdown(samples []promSample) statusBreakdown {
	sb := statusBreakdown{ByClass: map[string]float64{}}
	agg := map[string]float64{}
	for _, s := range samples {
		st := s.label("status")
		if st == "" {
			st = "unknown"
		}
		agg[st] += s.Value
		sb.Total += s.Value
		sb.ByClass[statusClass(st)] += s.Value
	}
	for st, v := range agg {
		sb.ByStatus = append(sb.ByStatus, statusCount{Status: st, Count: v})
	}
	sort.Slice(sb.ByStatus, func(i, j int) bool {
		if sb.ByStatus[i].Count != sb.ByStatus[j].Count {
			return sb.ByStatus[i].Count > sb.ByStatus[j].Count
		}
		return sb.ByStatus[i].Status < sb.ByStatus[j].Status
	})
	return sb
}

func statusClass(status string) string {
	if len(status) == 0 {
		return "other"
	}
	switch status[0] {
	case '1', '2', '3', '4', '5':
		return string(status[0]) + "xx"
	}
	return "other"
}

func (sb statusBreakdown) pct(v float64) float64 {
	if sb.Total <= 0 {
		return 0
	}
	return v / sb.Total * 100
}

// statusMeaning explains what a status code usually means when APIcast returns
// it, which is what turns a number into a troubleshooting lead.
func statusMeaning(status string) string {
	switch status {
	case "200", "201", "202", "204":
		return "success"
	case "301", "302", "304":
		return "redirect / not modified"
	case "400":
		return "bad request forwarded by the upstream API"
	case "401":
		return "unauthorized — OIDC token rejected, or upstream authentication"
	case "403":
		return "APIcast auth failure: missing/invalid credentials, or the app is suspended (see troubleshooting-apicast)"
	case "404":
		return "no mapping rule matched, unknown host, or the upstream returned 404"
	case "405":
		return "method not allowed by the mapping rules or the upstream"
	case "409":
		return "conflict — commonly a 3scale limits/usage conflict reported by backend"
	case "413":
		return "request body larger than the APIcast/router limit"
	case "429":
		return "rate limit exceeded: the application hit a plan limit or an APIcast rate-limit policy"
	case "499":
		return "client closed the connection before APIcast answered (often a client timeout on a slow upstream)"
	case "500":
		return "internal error — APIcast Lua error or upstream 500; check the gateway logs"
	case "502":
		return "bad gateway: upstream unreachable, TLS rejected, or an invalid response from the Private Base URL"
	case "503":
		return "service unavailable: no healthy upstream, or APIcast could not reach 3scale backend"
	case "504":
		return "gateway timeout: the upstream did not answer within the APIcast timeout"
	case "0", "unknown":
		return "no response recorded (connection aborted before a status was produced)"
	}
	switch statusClass(status) {
	case "4xx":
		return "client error"
	case "5xx":
		return "server error"
	}
	return ""
}

// ---- latency ----

type latency struct {
	Avg, P95, P99 float64
	Available     bool
}

func fetchLatency(ctx context.Context, d *deps, catalog map[string]bool, logical, selector, window string) latency {
	bucket := metricName(catalog, logical)
	sum := metricName(catalog, logical+"_sum")
	count := metricName(catalog, logical+"_count")
	if bucket == "" {
		return latency{}
	}
	queries := map[string]string{
		"p95": fmt.Sprintf("histogram_quantile(0.95, sum by (le) (rate(%s{%s}[%s])))", bucket, selector, window),
		"p99": fmt.Sprintf("histogram_quantile(0.99, sum by (le) (rate(%s{%s}[%s])))", bucket, selector, window),
	}
	if sum != "" && count != "" {
		queries["avg"] = fmt.Sprintf("sum(rate(%s{%s}[%s])) / sum(rate(%s{%s}[%s]))", sum, selector, window, count, selector, window)
	}
	res := runQueries(ctx, d.prom, queries)
	l := latency{}
	for k, r := range res {
		if r.err != nil || len(r.samples) == 0 {
			continue
		}
		v := r.samples[0].Value
		switch k {
		case "avg":
			l.Avg = v
		case "p95":
			l.P95 = v
		case "p99":
			l.P99 = v
		}
		l.Available = true
	}
	return l
}

func fmtSeconds(v float64) string {
	if v <= 0 || math.IsInf(v, 0) {
		return "-"
	}
	if v < 1 {
		return fmt.Sprintf("%.0f ms", v*1000)
	}
	return fmt.Sprintf("%.2f s", v)
}

// ---- trend ----

// trendPoint is one bucket of the request/error timeline.
type trendPoint struct {
	At    time.Time
	Total float64
	C4xx  float64
	C5xx  float64
}

func fetchTrend(ctx context.Context, d *deps, catalog map[string]bool, selector string, dur time.Duration) ([]trendPoint, error) {
	name := metricName(catalog, "upstream_status")
	if name == "" {
		return nil, fmt.Errorf("upstream_status not available")
	}
	buckets := 12
	step := dur / time.Duration(buckets)
	if step < 30*time.Second {
		step = 30 * time.Second
	}
	end := time.Now()
	start := end.Add(-dur)
	q := fmt.Sprintf("sum by (status) (rate(%s{%s}[%s]))", name, selector, promDuration(2*step))
	series, err := d.prom.rangeQuery(ctx, q, start, end, step)
	if err != nil {
		return nil, err
	}
	byTime := map[int64]*trendPoint{}
	for _, s := range series {
		class := statusClass(s.Labels["status"])
		for _, p := range s.Points {
			tp, ok := byTime[p.At.Unix()]
			if !ok {
				tp = &trendPoint{At: p.At}
				byTime[p.At.Unix()] = tp
			}
			// rate() is per second; multiply by the step to get requests per bucket.
			v := p.Value * step.Seconds()
			tp.Total += v
			switch class {
			case "4xx":
				tp.C4xx += v
			case "5xx":
				tp.C5xx += v
			}
		}
	}
	out := make([]trendPoint, 0, len(byTime))
	for _, v := range byTime {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func promDuration(d time.Duration) string {
	s := int(d.Seconds())
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s) + "s"
}

// renderTrend draws a compact ASCII timeline: enough for the model to spot a
// spike or a traffic drop without another round-trip.
func renderTrend(points []trendPoint) string {
	if len(points) == 0 {
		return "  (no data points)\n"
	}
	maxV := 0.0
	for _, p := range points {
		if p.Total > maxV {
			maxV = p.Total
		}
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  TIME\tREQUESTS\t4xx\t5xx\tSHAPE")
	for _, p := range points {
		bars := 0
		if maxV > 0 {
			bars = int(math.Round(p.Total / maxV * 24))
		}
		fmt.Fprintf(w, "  %s\t%.0f\t%.0f\t%.0f\t%s\n",
			p.At.UTC().Format("15:04:05Z"), p.Total, p.C4xx, p.C5xx, strings.Repeat("#", bars))
	}
	w.Flush()
	return b.String()
}

// ---- gateway attribution ----

// deploymentFromPod strips the ReplicaSet hash and pod suffix so traffic can
// be attributed to a gateway Deployment.
func deploymentFromPod(pod string) string {
	parts := strings.Split(pod, "-")
	if len(parts) <= 2 {
		return pod
	}
	return strings.Join(parts[:len(parts)-2], "-")
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- correlation with cluster state ----

// gatewayHealth summarises restarts and recent warnings for the gateways
// serving an API, so a 5xx spike can be tied to a crashing pod.
func gatewayHealth(ctx context.Context, d *deps, namespaces []string) string {
	if len(namespaces) == 0 || d.kc == nil || d.kc.clientset == nil {
		return ""
	}
	var b strings.Builder
	for _, ns := range namespaces {
		pods, err := d.kc.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			fmt.Fprintf(&b, "  %s: could not list pods: %v\n", ns, err)
			continue
		}
		found := false
		for i := range pods.Items {
			p := &pods.Items[i]
			if !strings.Contains(p.Name, "apicast") {
				continue
			}
			found = true
			ready, total, restarts, issues := podContainerSummary(p)
			line := fmt.Sprintf("  %s/%s: phase=%s ready=%d/%d restarts=%d age=%s",
				ns, p.Name, p.Status.Phase, ready, total, restarts, age(p.CreationTimestamp))
			if len(issues) > 0 {
				line += " issues=[" + strings.Join(issues, "; ") + "]"
			}
			b.WriteString(line + "\n")
		}
		if !found {
			fmt.Fprintf(&b, "  %s: no APIcast pods found\n", ns)
		}
	}
	return b.String()
}

// ---- self-diagnosis ----

// explainNoMetrics turns "no data" into an actionable answer. Instead of
// asserting that APIcast exports nothing, it reports the scope that was
// queried, the query that was run, which metric names DO exist there, and
// whether the per-API labels carry any values — which is enough to tell a
// scoping mistake from a missing ServiceMonitor from disabled extended
// metrics.
func explainNoMetrics(ctx context.Context, d *deps, sc metricScope, window string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no APIcast traffic metric (upstream_status) was found in the last %s.\n", window)
	fmt.Fprintf(&b, "Scope queried: %s\n", sc.describe())
	fmt.Fprintf(&b, "Probe: GET /api/v1/label/__name__/values?match[]={__name__=~\"upstream_status|...\"%s} over the last %s\n",
		prefixComma(sc.matchers()), window)

	// What metrics does this scope actually have?
	if names, err := d.prom.metricNamesIn(ctx, sc.matchers(), window); err != nil {
		fmt.Fprintf(&b, "Could not enumerate the metric names in this scope: %v\n", err)
	} else if len(names) == 0 {
		b.WriteString("\nNo gateway-like metric exists in this scope at all. Nothing is scraping APIcast:\n" +
			"  - is user workload monitoring enabled on the cluster?\n" +
			"  - is there a ServiceMonitor/PodMonitor in the gateway namespace, targeting the 9421 metrics port?\n" +
			"  - does the gateway Service actually expose that port?\n" +
			"Run 3scale_check_metrics_pipeline for a definitive answer.\n")
	} else {
		fmt.Fprintf(&b, "\nGateway-like metrics that DO exist in this scope: %s\n", strings.Join(names, ", "))
		b.WriteString("The traffic counter is named differently than expected in this build of APIcast. " +
			"Use 3scale_query_metrics with one of the names above to inspect it, and report the name so the tool can be taught it.\n")
	}

	// What labels does the traffic metric actually carry?
	if names, err := d.prom.labelNames(ctx, nil, time.Now().Add(-24*time.Hour), time.Now()); err == nil && len(names) > 0 {
		var svc []string
		for _, n := range names {
			if strings.Contains(n, "service") {
				svc = append(svc, n)
			}
		}
		if len(svc) > 0 {
			fmt.Fprintf(&b, "Service-related label names present in Prometheus: %s\n", strings.Join(svc, ", "))
		}
	}

	// Do the per-API labels carry anything, anywhere?
	for _, label := range []string{"service_system_name", "service_id"} {
		vals, err := d.prom.labelValues(ctx, label, nil, time.Now().Add(-24*time.Hour), time.Now())
		if err != nil {
			continue
		}
		if len(vals) == 0 {
			fmt.Fprintf(&b, "The %s label has no values anywhere in the cluster over the last 24h: "+
				"APICAST_EXTENDED_METRICS is not enabled on any gateway.\n", label)
		} else {
			fmt.Fprintf(&b, "The %s label does have values cluster-wide (%s), so extended metrics are on somewhere — "+
				"the scope above is probably wrong. Retry with an explicit \"namespace\".\n",
				label, strings.Join(truncateList(vals, 10), ", "))
		}
		break
	}
	return strings.TrimRight(b.String(), "\n")
}

// prefixComma renders extra matchers inside a selector that already has one.
func prefixComma(s string) string {
	if s == "" {
		return ""
	}
	return "," + s
}

func truncateList(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return append(append([]string{}, in[:n]...), "…")
}
