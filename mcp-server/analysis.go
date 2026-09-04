package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Report builders. Each one turns raw Prometheus data into a narrative the
// model can reason about: numbers first, then findings that name the probable
// cause and the runbook to follow, then the next tool to call.

// finding is one automatic observation, ordered by severity.
type finding struct {
	Severity string // critical, warning, info
	Text     string
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	}
	return 2
}

func renderFindings(fs []finding) string {
	if len(fs) == 0 {
		return "  No anomaly detected by the built-in rules.\n"
	}
	sort.SliceStable(fs, func(i, j int) bool { return severityRank(fs[i].Severity) < severityRank(fs[j].Severity) })
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "  [%s] %s\n", strings.ToUpper(f.Severity), f.Text)
	}
	return b.String()
}

// ---- list APIs ----

type listAPIsInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"OPTIONAL and rarely needed: APIs and gateways are found cluster-wide. Pass this only to restrict to one namespace deliberately; if the API is served elsewhere the tool widens the search anyway and says so. Do NOT pass the APIManager namespace expecting to find gateways there - self-managed APIcast usually runs in other namespaces"`
	Gateway   string `json:"gateway,omitempty" jsonschema:"restrict to one APIcast Deployment, e.g. apicast-production (optional)"`
	Window    string `json:"window,omitempty" jsonschema:"time window such as 15m, 1h, 24h or 7d (optional, default 1h)"`
}

func listAPIs(ctx context.Context, d *deps, in listAPIsInput) (string, error) {
	if !d.prom.enabled() {
		return promDisabledMsg, nil
	}
	_, window, err := parseWindow(in.Window)
	if err != nil {
		return "", err
	}
	sc := resolveScope(ctx, d, in.Namespace, in.Gateway)
	catalogMetrics, err := d.prom.metricCatalog(ctx, sc.matchers(), window)
	if err != nil {
		return "", err
	}
	statusMetric := metricName(catalogMetrics, "upstream_status")
	if statusMetric == "" {
		return explainNoMetrics(ctx, d, sc, window), nil
	}

	sel := sc.matchers()
	queries := map[string]string{
		"total": fmt.Sprintf("sum by (service_id, service_system_name, namespace) (increase(%s{%s}[%s]))", statusMetric, sel, window),
		"c4xx":  fmt.Sprintf(`sum by (service_id, service_system_name) (increase(%s{%s}[%s]))`, statusMetric, joinSelectors(sel, `status=~"4.."`), window),
		"c5xx":  fmt.Sprintf(`sum by (service_id, service_system_name) (increase(%s{%s}[%s]))`, statusMetric, joinSelectors(sel, `status=~"5.."`), window),
	}
	res := runQueries(ctx, d.prom, queries)
	if res["total"].err != nil {
		return "", res["total"].err
	}
	// A namespace hint must never hide APIs: if it matched nothing, fall back
	// to the whole cluster. The APIManager namespace commonly has no gateways.
	var widened string
	if len(res["total"].samples) == 0 && len(sc.Namespaces) > 0 {
		wide := clusterScope(in.Gateway)
		wideSel := wide.matchers()
		res = runQueries(ctx, d.prom, map[string]string{
			"total": fmt.Sprintf("sum by (service_id, service_system_name, namespace) (increase(%s{%s}[%s]))", statusMetric, wideSel, window),
			"c4xx":  fmt.Sprintf(`sum by (service_id, service_system_name) (increase(%s{%s}[%s]))`, statusMetric, joinSelectors(wideSel, `status=~"4.."`), window),
			"c5xx":  fmt.Sprintf(`sum by (service_id, service_system_name) (increase(%s{%s}[%s]))`, statusMetric, joinSelectors(wideSel, `status=~"5.."`), window),
		})
		if len(res["total"].samples) > 0 {
			widened = fmt.Sprintf("No APIcast traffic in %s; widened the search to the whole cluster.",
				strings.Join(sc.Namespaces, ", "))
			sc = wide
		}
	}

	type row struct {
		id, sys    string
		namespaces map[string]bool
		total      float64
		c4xx, c5xx float64
	}
	rows := map[string]*row{}
	get := func(id, sys string) *row {
		k := id + "\x00" + sys
		r, ok := rows[k]
		if !ok {
			r = &row{id: id, sys: sys, namespaces: map[string]bool{}}
			rows[k] = r
		}
		return r
	}
	for _, s := range res["total"].samples {
		r := get(s.label("service_id"), s.label("service_system_name"))
		r.total += s.Value
		if ns := s.label("namespace"); ns != "" {
			r.namespaces[ns] = true
		}
	}
	for _, s := range res["c4xx"].samples {
		get(s.label("service_id"), s.label("service_system_name")).c4xx += s.Value
	}
	for _, s := range res["c5xx"].samples {
		get(s.label("service_id"), s.label("service_system_name")).c5xx += s.Value
	}

	catalog, endpoint, catErr := d.admin.services(ctx, adminNamespace(ctx, d, in.Namespace))
	byID := map[string]apiService{}
	bySys := map[string]apiService{}
	for _, s := range catalog {
		byID[s.ID] = s
		bySys[s.SystemName] = s
	}

	list := make([]*row, 0, len(rows))
	for _, r := range rows {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].total > list[j].total })

	var b strings.Builder
	fmt.Fprintf(&b, "APIs served by APIcast in the last %s (%s):\n", window, sc.describe())
	if widened != "" {
		fmt.Fprintf(&b, "%s\n", widened)
	}
	b.WriteString("\n")
	if len(list) == 0 {
		b.WriteString("No traffic recorded in this window.\n")
	} else {
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PRODUCT\tSYSTEM NAME\tSERVICE ID\tNAMESPACE(S)\tREQUESTS\t4xx\t5xx\tERROR RATE")
		unlabelled := false
		for _, r := range list {
			name := "-"
			if s, ok := byID[r.id]; ok {
				name = s.Name
			} else if s, ok := bySys[r.sys]; ok {
				name = s.Name
			}
			if r.id == "" && r.sys == "" {
				name = "<unlabelled>"
				unlabelled = true
			}
			errRate := 0.0
			if r.total > 0 {
				errRate = (r.c4xx + r.c5xx) / r.total * 100
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.0f\t%.0f\t%.0f\t%.1f%%\n",
				name, orDash(r.sys), orDash(r.id), orDash(strings.Join(keysOf(r.namespaces), ",")),
				r.total, r.c4xx, r.c5xx, errRate)
		}
		w.Flush()
		if unlabelled {
			b.WriteString("\n<unlabelled> means traffic served by gateways running without APICAST_EXTENDED_METRICS=true: " +
				"those requests cannot be attributed to an API. Run 3scale_list_apicast_gateways to find them.\n")
		}
	}

	// Products configured but idle: often the real answer to "my API is down".
	if catErr == nil && len(catalog) > 0 {
		seen := map[string]bool{}
		for _, r := range list {
			seen[r.id] = true
			seen[r.sys] = true
		}
		var idle []string
		for _, s := range catalog {
			if !seen[s.ID] && !seen[s.SystemName] {
				idle = append(idle, fmt.Sprintf("%s (%s, id %s)", s.Name, s.SystemName, s.ID))
			}
		}
		fmt.Fprintf(&b, "\n3scale product catalog: %d products from %s.\n", len(catalog), endpoint)
		if len(idle) > 0 {
			fmt.Fprintf(&b, "Configured but with no traffic in this window: %s\n", strings.Join(idle, ", "))
		}
	} else if catErr != nil {
		fmt.Fprintf(&b, "\nProduct names unavailable: %s\n", firstLine(catErr.Error()))
	}
	b.WriteString("\nNext: 3scale_analyze_api_metrics with the product name, system name or service id for a full breakdown.\n")
	return b.String(), nil
}

// ---- analyze one API ----

type analyzeAPIInput struct {
	API       string `json:"api" jsonschema:"the API to analyse: product name as shown in the Admin Portal, its system name, or its numeric service id"`
	Namespace string `json:"namespace,omitempty" jsonschema:"OPTIONAL and rarely needed: APIs and gateways are found cluster-wide. Pass this only to restrict to one namespace deliberately; if the API is served elsewhere the tool widens the search anyway and says so. Do NOT pass the APIManager namespace expecting to find gateways there - self-managed APIcast usually runs in other namespaces"`
	Gateway   string `json:"gateway,omitempty" jsonschema:"restrict to one APIcast Deployment, e.g. apicast-production or apicast-staging (optional)"`
	Window    string `json:"window,omitempty" jsonschema:"time window such as 15m, 1h, 24h or 7d (optional, default 1h)"`
	NoTrend   bool   `json:"noTrend,omitempty" jsonschema:"skip the request/error timeline (optional)"`
}

func analyzeAPI(ctx context.Context, d *deps, in analyzeAPIInput) (string, error) {
	if !d.prom.enabled() {
		return promDisabledMsg, nil
	}
	dur, window, err := parseWindow(in.Window)
	if err != nil {
		return "", err
	}
	sc := resolveScope(ctx, d, in.Namespace, in.Gateway)
	api, err := resolveAPI(ctx, d, in.API, in.Namespace, window, sc)
	if err != nil {
		return "", err
	}
	// Now that the API has been located cluster-wide, narrow the metric
	// queries to the namespaces actually serving it — precise, and without
	// ever having used a namespace as a filter while searching.
	//
	// A namespace argument is a hint, never a blindfold. In the usual topology
	// the APIManager lives in one namespace and the APIcast gateways in
	// others, so a caller naming "the 3scale namespace" would otherwise get an
	// empty report for an API that is plainly serving traffic next door.
	var scopeNote string
	if len(api.Namespaces) > 0 {
		if in.Namespace != "" && !contains(api.Namespaces, in.Namespace) {
			scopeNote = fmt.Sprintf("You asked for namespace %q, but this API is served from %s — "+
				"reporting on where its traffic actually is. (In a typical install the APIManager and the APIcast "+
				"gateways are in different namespaces.)", in.Namespace, strings.Join(api.Namespaces, ", "))
		}
		sc = metricScope{
			Namespaces: api.Namespaces,
			Gateway:    in.Gateway,
			Origin:     "namespaces where this API's traffic was found",
		}
	}
	catalogMetrics, err := d.prom.metricCatalog(ctx, sc.matchers(), window)
	if err != nil {
		return "", err
	}
	statusMetric := metricName(catalogMetrics, "upstream_status")
	if statusMetric == "" {
		return explainNoMetrics(ctx, d, sc, window), nil
	}

	apiSel := api.selector()
	sel := joinSelectors(sc.matchers(), apiSel)

	queries := map[string]string{
		"status":    fmt.Sprintf("sum by (status) (increase(%s{%s}[%s]))", statusMetric, sel, window),
		"byGateway": fmt.Sprintf("sum by (namespace, pod) (increase(%s{%s}[%s]))", statusMetric, sel, window),
		"errByGateway": fmt.Sprintf(`sum by (namespace, pod) (increase(%s{%s}[%s]))`,
			statusMetric, joinSelectors(sel, `status=~"5.."`), window),
	}
	if backend := metricName(catalogMetrics, "threescale_backend_calls"); backend != "" {
		queries["backend"] = fmt.Sprintf("sum by (endpoint, status) (increase(%s{%s}[%s]))", backend, sc.matchers(), window)
	}
	res := runQueries(ctx, d.prom, queries)
	if res["status"].err != nil {
		return "", res["status"].err
	}
	sb := newStatusBreakdown(res["status"].samples)

	totalLat := fetchLatency(ctx, d, catalogMetrics, "total_response_time", sel, window)
	upstreamLat := fetchLatency(ctx, d, catalogMetrics, "upstream_response_time", sel, window)

	var b strings.Builder
	fmt.Fprintf(&b, "=== API metrics: %s ===\n", api.title())
	fmt.Fprintf(&b, "Window: last %s | Scope: %s | Matched by: %s\n", window, sc.describe(), api.MatchedBy)
	if api.CatalogNote != "" {
		fmt.Fprintf(&b, "%s\n", api.CatalogNote)
	}
	if len(api.Namespaces) > 0 {
		fmt.Fprintf(&b, "Served from namespace(s): %s\n", strings.Join(api.Namespaces, ", "))
	}
	if scopeNote != "" {
		fmt.Fprintf(&b, "\n%s\n", scopeNote)
	}
	if !api.HasTraffic {
		b.WriteString("\nThis product exists in 3scale but produced NO traffic in this window.\n" +
			"Either it is genuinely idle, or requests never reach APIcast (route/DNS/host mismatch), " +
			"or the gateway serving it lacks APICAST_EXTENDED_METRICS so its traffic is unattributed.\n")
	}

	// --- volume and status codes ---
	rps := 0.0
	if dur > 0 {
		rps = sb.Total / dur.Seconds()
	}
	fmt.Fprintf(&b, "\n--- Traffic ---\n  total requests: %.0f (%.2f req/s average)\n", sb.Total, rps)
	if sb.Total > 0 {
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  CLASS\tREQUESTS\tSHARE")
		for _, c := range []string{"2xx", "3xx", "4xx", "5xx", "1xx", "other"} {
			if v, ok := sb.ByClass[c]; ok && v > 0 {
				fmt.Fprintf(w, "  %s\t%.0f\t%.1f%%\n", c, v, sb.pct(v))
			}
		}
		w.Flush()

		b.WriteString("\n--- Status codes ---\n")
		w = tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  STATUS\tREQUESTS\tSHARE\tTYPICAL CAUSE IN APICAST")
		for _, s := range sb.ByStatus {
			fmt.Fprintf(w, "  %s\t%.0f\t%.1f%%\t%s\n", s.Status, s.Count, sb.pct(s.Count), orDash(statusMeaning(s.Status)))
		}
		w.Flush()
	}

	// --- per gateway ---
	if len(res["byGateway"].samples) > 0 {
		type gw struct{ total, errors float64 }
		agg := map[string]*gw{}
		for _, s := range res["byGateway"].samples {
			k := s.label("namespace") + "/" + deploymentFromPod(s.label("pod"))
			if agg[k] == nil {
				agg[k] = &gw{}
			}
			agg[k].total += s.Value
		}
		for _, s := range res["errByGateway"].samples {
			k := s.label("namespace") + "/" + deploymentFromPod(s.label("pod"))
			if agg[k] == nil {
				agg[k] = &gw{}
			}
			agg[k].errors += s.Value
		}
		b.WriteString("\n--- Per gateway ---\n")
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE/DEPLOYMENT\tREQUESTS\t5xx\t5xx RATE")
		for _, k := range sortedCopy(keysOfGw(agg)) {
			v := agg[k]
			rate := 0.0
			if v.total > 0 {
				rate = v.errors / v.total * 100
			}
			fmt.Fprintf(w, "  %s\t%.0f\t%.0f\t%.1f%%\n", k, v.total, v.errors, rate)
		}
		w.Flush()
	}

	// --- latency ---
	b.WriteString("\n--- Latency ---\n")
	if totalLat.Available || upstreamLat.Available {
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  MEASURE\tAVG\tP95\tP99")
		fmt.Fprintf(w, "  total (client-observed)\t%s\t%s\t%s\n", fmtSeconds(totalLat.Avg), fmtSeconds(totalLat.P95), fmtSeconds(totalLat.P99))
		fmt.Fprintf(w, "  upstream (your backend API)\t%s\t%s\t%s\n", fmtSeconds(upstreamLat.Avg), fmtSeconds(upstreamLat.P95), fmtSeconds(upstreamLat.P99))
		if totalLat.P95 > 0 && upstreamLat.P95 > 0 {
			fmt.Fprintf(w, "  APIcast overhead (p95)\t\t%s\t\n", fmtSeconds(totalLat.P95-upstreamLat.P95))
		}
		w.Flush()
	} else {
		b.WriteString("  Response-time histograms are not available for this API.\n")
	}

	// --- 3scale backend calls (gateway-wide) ---
	if bs, ok := res["backend"]; ok && bs.err == nil && len(bs.samples) > 0 {
		b.WriteString("\n--- Calls from APIcast to 3scale backend (gateway-wide, not per API) ---\n")
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  ENDPOINT\tSTATUS\tCALLS")
		type bc struct {
			endpoint, status string
			v                float64
		}
		var rowsB []bc
		for _, s := range bs.samples {
			rowsB = append(rowsB, bc{s.label("endpoint"), s.label("status"), s.Value})
		}
		sort.Slice(rowsB, func(i, j int) bool { return rowsB[i].v > rowsB[j].v })
		for _, r := range rowsB {
			fmt.Fprintf(w, "  %s\t%s\t%.0f\n", orDash(r.endpoint), orDash(r.status), r.v)
		}
		w.Flush()
		b.WriteString("  (authrep/authorize failures here mean APIcast cannot validate credentials: check backend-listener and backend-redis.)\n")
	}

	// --- integration settings ---
	// The public endpoint and the Private Base URL are what 404s and 502s are
	// usually about, so surface them next to the status codes.
	if api.ID != "" {
		if proxy, perr := d.admin.proxyConfig(ctx, adminNamespace(ctx, d, in.Namespace), api.ID); perr == nil && len(proxy) > 0 {
			b.WriteString("\n--- Integration (from the 3scale Admin API) ---\n")
			w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
			for _, k := range sortedCopy(keysOfGw(proxy)) {
				fmt.Fprintf(w, "  %s\t%s\n", k, proxy[k])
			}
			w.Flush()
			b.WriteString("  (endpoint = public production URL, sandbox_endpoint = staging, api_backend = Private Base URL of your upstream.)\n")
		}
	}

	// --- trend ---
	if !in.NoTrend {
		b.WriteString("\n--- Timeline ---\n")
		points, terr := fetchTrend(ctx, d, catalogMetrics, sel, dur)
		if terr != nil {
			fmt.Fprintf(&b, "  unavailable: %v\n", terr)
		} else {
			b.WriteString(renderTrend(points))
		}
	}

	// --- gateway pods ---
	nss := api.Namespaces
	if len(nss) == 0 && in.Namespace != "" {
		nss = []string{in.Namespace}
	}
	if h := gatewayHealth(ctx, d, nss); h != "" {
		b.WriteString("\n--- APIcast pods serving this API ---\n" + h)
	}

	// --- findings ---
	b.WriteString("\n--- Findings ---\n")
	b.WriteString(renderFindings(apiFindings(sb, totalLat, upstreamLat, res)))
	b.WriteString("\nNext steps: 3scale_get_pod_logs on the APIcast pod above for Lua/upstream errors, " +
		"3scale_check_routes for host and TLS problems, 3scale_check_database_config when backend calls fail.\n")
	return b.String(), nil
}

func keysOfGw[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// apiFindings encodes the interpretation rules that turn the numbers above
// into probable causes.
func apiFindings(sb statusBreakdown, totalLat, upstreamLat latency, res map[string]queryResult) []finding {
	var fs []finding
	if sb.Total == 0 {
		return append(fs, finding{"warning", "No requests were recorded for this API in the window."})
	}
	c5 := sb.pct(sb.ByClass["5xx"])
	c4 := sb.pct(sb.ByClass["4xx"])
	byStatus := map[string]float64{}
	for _, s := range sb.ByStatus {
		byStatus[s.Status] = sb.pct(s.Count)
	}

	switch {
	case c5 >= 20:
		fs = append(fs, finding{"critical", fmt.Sprintf("%.1f%% of responses are 5xx — the API is largely failing.", c5)})
	case c5 >= 5:
		fs = append(fs, finding{"warning", fmt.Sprintf("%.1f%% of responses are 5xx, above the 5%% threshold.", c5)})
	case c5 > 0:
		fs = append(fs, finding{"info", fmt.Sprintf("%.1f%% of responses are 5xx.", c5)})
	}
	if byStatus["502"] > 1 {
		fs = append(fs, finding{"critical", fmt.Sprintf("502 Bad Gateway on %.1f%% of requests: APIcast cannot get a valid response from the Private Base URL. "+
			"Verify the upstream is up, that its DNS name resolves from the gateway pod, and that its TLS certificate is trusted (see troubleshooting-apicast).", byStatus["502"])})
	}
	if byStatus["503"] > 1 {
		fs = append(fs, finding{"critical", fmt.Sprintf("503 on %.1f%% of requests: no healthy upstream, or APIcast could not reach 3scale backend to authorise the call.", byStatus["503"])})
	}
	if byStatus["504"] > 1 {
		fs = append(fs, finding{"warning", fmt.Sprintf("504 Gateway Timeout on %.1f%% of requests: the upstream exceeds the APIcast timeout. Compare with the upstream p99 below.", byStatus["504"])})
	}
	if byStatus["403"] > 5 {
		fs = append(fs, finding{"warning", fmt.Sprintf("403 on %.1f%% of requests: credentials missing/invalid, application suspended, or OIDC tokens rejected. "+
			"Check the credential location in the product integration and whether zync synchronised the OIDC client.", byStatus["403"])})
	}
	if byStatus["404"] > 5 {
		fs = append(fs, finding{"warning", fmt.Sprintf("404 on %.1f%% of requests: usually no mapping rule matched the path, or the request host is not a configured public endpoint. "+
			"Remember production APIcast only reloads configuration every APICAST_CONFIGURATION_CACHE seconds after a promote.", byStatus["404"])})
	}
	if byStatus["429"] > 1 {
		fs = append(fs, finding{"warning", fmt.Sprintf("429 on %.1f%% of requests: applications are hitting plan limits or an APIcast rate-limit policy.", byStatus["429"])})
	}
	if byStatus["499"] > 5 {
		fs = append(fs, finding{"warning", fmt.Sprintf("499 on %.1f%% of requests: clients are giving up before APIcast answers — nearly always a slow upstream.", byStatus["499"])})
	}
	if c4 >= 30 && byStatus["403"] <= 5 && byStatus["404"] <= 5 {
		fs = append(fs, finding{"warning", fmt.Sprintf("%.1f%% of responses are 4xx, spread over several codes: likely client-side or contract problems rather than a gateway fault.", c4)})
	}

	if totalLat.P95 > 0 && upstreamLat.P95 > 0 {
		overhead := totalLat.P95 - upstreamLat.P95
		if overhead > 0.5 && overhead > upstreamLat.P95 {
			fs = append(fs, finding{"warning", fmt.Sprintf("APIcast adds %s at p95 on top of the upstream — more than the upstream itself. "+
				"Look at authrep latency to 3scale backend (backend-listener/backend-redis), heavy policies, or configuration reloads.", fmtSeconds(overhead))})
		}
	}
	if upstreamLat.P99 > 5 {
		fs = append(fs, finding{"warning", fmt.Sprintf("Upstream p99 is %s: the backend API itself is slow, which also drives 504 and 499 responses.", fmtSeconds(upstreamLat.P99))})
	}

	if bs, ok := res["backend"]; ok && bs.err == nil {
		var total, failed float64
		for _, s := range bs.samples {
			total += s.Value
			if statusClass(s.label("status")) != "2xx" {
				failed += s.Value
			}
		}
		if total > 0 && failed/total > 0.01 {
			fs = append(fs, finding{"critical", fmt.Sprintf("%.1f%% of the calls APIcast makes to 3scale backend are not 2xx. "+
				"Authorisation is degraded cluster-wide, not just for this API: check backend-listener pods and the backend Redis connection.", failed/total*100)})
		}
	}
	if len(fs) == 0 {
		fs = append(fs, finding{"info", fmt.Sprintf("Traffic looks healthy: %.1f%% 2xx, no dominant error code.", sb.pct(sb.ByClass["2xx"]))})
	}
	return fs
}

// ---- traffic overview ----

type overviewInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"OPTIONAL and rarely needed: APIs and gateways are found cluster-wide. Pass this only to restrict to one namespace deliberately; if the API is served elsewhere the tool widens the search anyway and says so. Do NOT pass the APIManager namespace expecting to find gateways there - self-managed APIcast usually runs in other namespaces"`
	Window    string `json:"window,omitempty" jsonschema:"time window such as 15m, 1h, 24h or 7d (optional, default 1h)"`
}

func trafficOverview(ctx context.Context, d *deps, in overviewInput) (string, error) {
	if !d.prom.enabled() {
		return promDisabledMsg, nil
	}
	_, window, err := parseWindow(in.Window)
	if err != nil {
		return "", err
	}
	sc := resolveScope(ctx, d, in.Namespace, "")
	sel := sc.matchers()
	catalogMetrics, err := d.prom.metricCatalog(ctx, sel, window)
	if err != nil {
		return "", err
	}
	statusMetric := metricName(catalogMetrics, "upstream_status")
	if statusMetric == "" {
		return explainNoMetrics(ctx, d, sc, window), nil
	}

	queries := map[string]string{
		"byGateway":    fmt.Sprintf("sum by (namespace, pod) (increase(%s{%s}[%s]))", statusMetric, sel, window),
		"errByGateway": fmt.Sprintf(`sum by (namespace, pod) (increase(%s{%s}[%s]))`, statusMetric, joinSelectors(sel, `status=~"5.."`), window),
		"cliByGateway": fmt.Sprintf(`sum by (namespace, pod) (increase(%s{%s}[%s]))`, statusMetric, joinSelectors(sel, `status=~"4.."`), window),
		"byStatus":     fmt.Sprintf("sum by (status) (increase(%s{%s}[%s]))", statusMetric, sel, window),
		"topErrAPIs":   fmt.Sprintf(`topk(10, sum by (service_system_name, service_id) (increase(%s{%s}[%s])))`, statusMetric, joinSelectors(sel, `status=~"5.."`), window),
	}
	if backend := metricName(catalogMetrics, "threescale_backend_calls"); backend != "" {
		queries["backend"] = fmt.Sprintf("sum by (namespace, endpoint, status) (increase(%s{%s}[%s]))", backend, sel, window)
	}
	if conns := metricName(catalogMetrics, "nginx_http_connections"); conns != "" {
		queries["connections"] = fmt.Sprintf("sum by (namespace, state) (%s{%s})", conns, sel)
	}
	if errlog := metricName(catalogMetrics, "nginx_error_log"); errlog != "" {
		queries["errorlog"] = fmt.Sprintf("sum by (namespace, level) (increase(%s{%s}[%s]))", errlog, sel, window)
	}
	if free, capacity := metricName(catalogMetrics, "openresty_shdict_free_space"), metricName(catalogMetrics, "openresty_shdict_capacity"); free != "" && capacity != "" {
		queries["shdict"] = fmt.Sprintf("min by (namespace, dict) (%s{%s} / %s{%s})", free, sel, capacity, sel)
	}
	res := runQueries(ctx, d.prom, queries)
	if len(res["byStatus"].samples) == 0 && len(sc.Namespaces) > 0 {
		// Same rule as elsewhere: a namespace hint must not hide the fleet.
		if wide, wres, ok := retryClusterWide(ctx, d, queries, sc); ok {
			sc, res = wide, wres
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "=== APIcast traffic overview (last %s, %s) ===\n", window, sc.describe())

	sb := newStatusBreakdown(res["byStatus"].samples)
	fmt.Fprintf(&b, "\nTotal requests: %.0f\n", sb.Total)
	if sb.Total > 0 {
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  CLASS\tREQUESTS\tSHARE")
		for _, c := range []string{"2xx", "3xx", "4xx", "5xx", "other"} {
			if v, ok := sb.ByClass[c]; ok && v > 0 {
				fmt.Fprintf(w, "  %s\t%.0f\t%.1f%%\n", c, v, sb.pct(v))
			}
		}
		w.Flush()
	}

	// per gateway
	type gwRow struct{ total, e5, e4 float64 }
	gws := map[string]*gwRow{}
	acc := func(samples []promSample, f func(*gwRow, float64)) {
		for _, s := range samples {
			k := s.label("namespace") + "/" + deploymentFromPod(s.label("pod"))
			if gws[k] == nil {
				gws[k] = &gwRow{}
			}
			f(gws[k], s.Value)
		}
	}
	acc(res["byGateway"].samples, func(r *gwRow, v float64) { r.total += v })
	acc(res["errByGateway"].samples, func(r *gwRow, v float64) { r.e5 += v })
	acc(res["cliByGateway"].samples, func(r *gwRow, v float64) { r.e4 += v })
	if len(gws) > 0 {
		b.WriteString("\n--- Per gateway ---\n")
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE/DEPLOYMENT\tREQUESTS\t4xx\t5xx\t5xx RATE")
		for _, k := range sortedCopy(keysOfGw(gws)) {
			r := gws[k]
			rate := 0.0
			if r.total > 0 {
				rate = r.e5 / r.total * 100
			}
			fmt.Fprintf(w, "  %s\t%.0f\t%.0f\t%.0f\t%.1f%%\n", k, r.total, r.e4, r.e5, rate)
		}
		w.Flush()
	}

	if s := res["topErrAPIs"].samples; len(s) > 0 {
		b.WriteString("\n--- APIs producing the most 5xx ---\n")
		sort.Slice(s, func(i, j int) bool { return s[i].Value > s[j].Value })
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  SYSTEM NAME\tSERVICE ID\t5xx")
		for _, x := range s {
			if x.Value <= 0 {
				continue
			}
			fmt.Fprintf(w, "  %s\t%s\t%.0f\n", orDash(x.label("service_system_name")), orDash(x.label("service_id")), x.Value)
		}
		w.Flush()
	}

	if s := res["backend"].samples; len(s) > 0 {
		b.WriteString("\n--- Calls to 3scale backend (authrep) ---\n")
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tENDPOINT\tSTATUS\tCALLS")
		sort.Slice(s, func(i, j int) bool { return s[i].Value > s[j].Value })
		for _, x := range s {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%.0f\n", orDash(x.label("namespace")), orDash(x.label("endpoint")), orDash(x.label("status")), x.Value)
		}
		w.Flush()
	}

	if s := res["errorlog"].samples; len(s) > 0 {
		b.WriteString("\n--- nginx error log entries by level ---\n")
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tLEVEL\tENTRIES")
		for _, x := range s {
			if x.Value <= 0 {
				continue
			}
			fmt.Fprintf(w, "  %s\t%s\t%.0f\n", orDash(x.label("namespace")), orDash(x.label("level")), x.Value)
		}
		w.Flush()
	}

	if s := res["connections"].samples; len(s) > 0 {
		b.WriteString("\n--- nginx connections ---\n")
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE\tSTATE\tCONNECTIONS")
		for _, x := range s {
			fmt.Fprintf(w, "  %s\t%s\t%.0f\n", orDash(x.label("namespace")), orDash(x.label("state")), x.Value)
		}
		w.Flush()
	}

	if s := res["shdict"].samples; len(s) > 0 {
		var low []string
		for _, x := range s {
			if x.Value < 0.1 {
				low = append(low, fmt.Sprintf("%s/%s at %.0f%% free", x.label("namespace"), x.label("dict"), x.Value*100))
			}
		}
		if len(low) > 0 {
			fmt.Fprintf(&b, "\n--- Shared dictionaries nearly full ---\n  %s\n"+
				"  A full configuration dictionary makes APIcast drop or fail to load service configuration.\n", strings.Join(low, ", "))
		}
	}

	var fs []finding
	if sb.Total > 0 {
		if r := sb.pct(sb.ByClass["5xx"]); r >= 5 {
			fs = append(fs, finding{"critical", fmt.Sprintf("Cluster-wide 5xx rate is %.1f%%.", r)})
		}
		if r := sb.pct(sb.ByClass["4xx"]); r >= 30 {
			fs = append(fs, finding{"warning", fmt.Sprintf("Cluster-wide 4xx rate is %.1f%% — check credentials and mapping rules.", r)})
		}
	} else {
		fs = append(fs, finding{"warning", "No APIcast traffic recorded in this window."})
	}
	b.WriteString("\n--- Findings ---\n" + renderFindings(fs))
	b.WriteString("\nNext: 3scale_list_apis to rank APIs, then 3scale_analyze_api_metrics on the worst one.\n")
	return b.String(), nil
}

// retryClusterWide re-runs a query set with the namespace matcher removed. It
// is used whenever a namespace hint produced nothing, so that a caller naming
// the APIManager namespace still sees the gateways in other namespaces.
func retryClusterWide(ctx context.Context, d *deps, queries map[string]string, sc metricScope) (metricScope, map[string]queryResult, bool) {
	narrow := sc.matchers()
	if narrow == "" {
		return sc, nil, false
	}
	wide := clusterScope(sc.Gateway)
	rewritten := make(map[string]string, len(queries))
	for name, q := range queries {
		rewritten[name] = stripMatcher(q, narrow)
	}
	res := runQueries(ctx, d.prom, rewritten)
	for _, r := range res {
		if r.err == nil && len(r.samples) > 0 {
			wide.Origin = "widened from " + strings.Join(sc.Namespaces, ", ") + ": no traffic there"
			return wide, res, true
		}
	}
	return sc, nil, false
}

// stripMatcher removes a label matcher from every selector in a PromQL
// expression without leaving the dangling commas that would make the result
// unparseable ("{,status=~\"5..\"}").
func stripMatcher(query, matcher string) string {
	if matcher == "" {
		return query
	}
	out := query
	for _, form := range []string{matcher + ",", "," + matcher, matcher} {
		out = strings.ReplaceAll(out, form, "")
	}
	return out
}

// ---- metrics pipeline check ----

func checkMetricsPipeline(ctx context.Context, d *deps, in nsInput) (string, error) {
	var b strings.Builder
	b.WriteString("=== APIcast metrics pipeline check ===\n\n")
	ok, problems := true, 0

	// 1. Gateways and their metric settings.
	// Always cluster-wide: gateways are routinely deployed away from the
	// APIManager, and a namespace-filtered search would report "none found".
	gws, warns, err := discoverGateways(ctx, d.kc, "")
	b.WriteString("--- 1. APIcast gateways ---\n")
	if err != nil {
		problems++
		fmt.Fprintf(&b, "  error: %v\n", err)
	} else if len(gws) == 0 {
		problems++
		b.WriteString("  No APIcast gateway found in the cluster.\n")
	} else {
		w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NAMESPACE/DEPLOYMENT\tREADY\tEXT. METRICS\tMETRICS PORT\tMONITORS")
		for _, g := range gws {
			fmt.Fprintf(w, "  %s/%s\t%d/%d\t%s\t%s\t%s\n", g.Namespace, g.Deployment, g.Ready, g.Desired,
				yesNo(g.extendedMetrics()), yesNo(g.MetricsPort), orDash(strings.Join(g.Monitors, ",")))
		}
		w.Flush()
		for _, g := range gws {
			if !g.extendedMetrics() {
				problems++
				fmt.Fprintf(&b, "  %s/%s: APICAST_EXTENDED_METRICS is off — no per-API labels.\n", g.Namespace, g.Deployment)
			}
			if len(g.Monitors) == 0 {
				problems++
				fmt.Fprintf(&b, "  %s: no ServiceMonitor/PodMonitor in this namespace — Prometheus has nothing telling it to scrape port 9421.\n", g.Namespace)
			}
		}
		// A ServiceMonitor pointing at a port the Service does not expose
		// scrapes nothing, silently. Check the Services behind the gateways.
		for _, ns := range gatewayNamespaces(gws) {
			exposed, err := metricsPortServices(ctx, d, ns)
			switch {
			case err != nil:
				fmt.Fprintf(&b, "  %s: could not list Services: %v\n", ns, err)
			case len(exposed) == 0:
				problems++
				fmt.Fprintf(&b, "  %s: no Service exposes the APIcast metrics port 9421 — a ServiceMonitor in this namespace "+
					"has no port to scrape. Add the port to the gateway Service, or use a PodMonitor instead.\n", ns)
			default:
				fmt.Fprintf(&b, "  %s: metrics port exposed by Service(s) %s\n", ns, strings.Join(exposed, ", "))
			}
		}
	}
	for _, w := range warns {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}

	// 2. User workload monitoring.
	b.WriteString("\n--- 2. OpenShift user workload monitoring ---\n")
	cm, err := d.kc.clientset.CoreV1().ConfigMaps("openshift-monitoring").Get(ctx, "cluster-monitoring-config", metav1.GetOptions{})
	switch {
	case err != nil:
		fmt.Fprintf(&b, "  could not read openshift-monitoring/cluster-monitoring-config: %v\n", err)
		b.WriteString("  (this only means the check is inconclusive; grant get on that ConfigMap for a definitive answer)\n")
	case strings.Contains(strings.ReplaceAll(cm.Data["config.yaml"], " ", ""), "enableUserWorkload:true"):
		b.WriteString("  enableUserWorkload: true — user workload monitoring is enabled.\n")
	default:
		problems++
		ok = false
		b.WriteString("  enableUserWorkload is NOT true: APIcast metrics are never collected.\n" +
			"  Fix: set enableUserWorkload: true in the openshift-monitoring/cluster-monitoring-config ConfigMap.\n")
	}

	// 3. Prometheus reachability and the APIcast metric catalog.
	b.WriteString("\n--- 3. Prometheus / Thanos ---\n")
	if !d.prom.enabled() {
		problems++
		fmt.Fprintf(&b, "  %s\n", promDisabledMsg)
	} else {
		fmt.Fprintf(&b, "  endpoint: %s\n", d.prom.baseURL)
		pipelineScope := resolveScope(ctx, d, in.Namespace, "")
		fmt.Fprintf(&b, "  scope: %s\n", pipelineScope.describe())
		catalog, cerr := d.prom.metricCatalog(ctx, pipelineScope.matchers(), "24h")
		if cerr != nil {
			problems++
			fmt.Fprintf(&b, "  query failed: %v\n", cerr)
		} else if len(catalog) == 0 {
			problems++
			b.WriteString("  reachable, but NO APIcast metric is present.\n" +
				"  Either nothing scrapes port 9421 yet, or no request has been served since the gateways started.\n")
		} else {
			names := keysOf(catalog)
			fmt.Fprintf(&b, "  reachable; APIcast metrics present: %s\n", strings.Join(names, ", "))
			for _, logical := range []string{"upstream_status", "total_response_time", "upstream_response_time", "threescale_backend_calls"} {
				if metricName(catalog, logical) == "" {
					fmt.Fprintf(&b, "  missing: %s (some analyses will be unavailable)\n", logical)
				}
			}
			// Are the labels for per-API analysis actually there?
			if metricName(catalog, "upstream_status") != "" {
				vals, verr := d.prom.labelValues(ctx, "service_system_name",
					[]string{metricName(catalog, "upstream_status")}, time.Now().Add(-24*time.Hour), time.Now())
				switch {
				case verr != nil:
					fmt.Fprintf(&b, "  could not read service_system_name label values: %v\n", verr)
				case len(vals) == 0:
					problems++
					b.WriteString("  the service_system_name label has no values: per-API analysis needs APICAST_EXTENDED_METRICS=true.\n")
				default:
					fmt.Fprintf(&b, "  per-API labels OK; APIs seen in the last 24h: %s\n", strings.Join(vals, ", "))
				}
			}
		}
		// Scrape targets.
		if len(gws) > 0 {
			var nss []string
			for _, g := range gws {
				nss = append(nss, escapeRegex(g.Namespace))
			}
			q := fmt.Sprintf(`up{namespace=~"%s"}`, strings.Join(dedupe(sortedCopy(nss)), "|"))
			if samples, uerr := d.prom.instant(ctx, q, time.Time{}); uerr == nil && len(samples) > 0 {
				down := 0
				for _, s := range samples {
					if s.Value == 0 {
						down++
						fmt.Fprintf(&b, "  target DOWN: %s/%s (job %s)\n", s.label("namespace"), s.label("pod"), s.label("job"))
					}
				}
				fmt.Fprintf(&b, "  scrape targets in gateway namespaces: %d, down: %d\n", len(samples), down)
				problems += down
			}
		}
	}

	// 4. 3scale product catalog (names).
	b.WriteString("\n--- 4. 3scale product catalog (for name lookups) ---\n")
	svcs, endpoint, aerr := d.admin.services(ctx, adminNamespace(ctx, d, in.Namespace))
	if aerr != nil {
		fmt.Fprintf(&b, "  unavailable: %v\n", firstLine(aerr.Error()))
		b.WriteString("  APIs can still be analysed by system name or service id.\n")
	} else {
		fmt.Fprintf(&b, "  %d products read from %s.\n", len(svcs), endpoint)
	}

	fmt.Fprintf(&b, "\n=== Verdict: %s ===\n", pipelineVerdict(problems, ok))
	if problems > 0 {
		b.WriteString(pipelineRemediation)
	}
	return b.String(), nil
}

// gatewayNamespaces lists the distinct namespaces holding gateways.
func gatewayNamespaces(gws []apicastGateway) []string {
	seen := map[string]bool{}
	for _, g := range gws {
		seen[g.Namespace] = true
	}
	return keysOf(seen)
}

// metricsPortServices returns the Services in ns that expose the APIcast
// metrics port, with the port name a ServiceMonitor must reference.
func metricsPortServices(ctx context.Context, d *deps, ns string) ([]string, error) {
	svcs, err := d.kc.clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		for _, p := range svc.Spec.Ports {
			if p.Port == 9421 || p.Name == "metrics" {
				out = append(out, fmt.Sprintf("%s (port %d, name %q)", svc.Name, p.Port, p.Name))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func pipelineVerdict(problems int, uwmOK bool) string {
	switch {
	case problems == 0:
		return "the metrics pipeline is healthy; per-API analysis is available"
	case !uwmOK:
		return "metrics are not being collected at all — fix user workload monitoring first"
	default:
		return fmt.Sprintf("%d problem(s) block or degrade per-API metric analysis", problems)
	}
}

const pipelineRemediation = `
Reference configuration:

1. Enable user workload monitoring (cluster admin, once per cluster):
     oc -n openshift-monitoring edit configmap cluster-monitoring-config
     data:
       config.yaml: |
         enableUserWorkload: true

2. Enable per-API labels on every gateway. What ultimately matters is that
   APICAST_EXTENDED_METRICS=true reaches the apicast container; the CR field
   that sets it depends on the operator version, so verify the result with
   3scale_list_apicast_gateways after applying.
   APIcast operator (kind APIcast):
     spec:
       extendedMetrics: true
   APIManager-managed gateways (recent 3scale operators):
     spec:
       apicast:
         stagingSpec:    { extendedMetrics: true }
         productionSpec: { extendedMetrics: true }
   If your CRD does not expose the field, check the operator documentation for
   the supported way to inject the variable — editing the Deployment directly
   is reverted by the operator on the next reconcile.

3. Scrape the gateway (one ServiceMonitor per namespace holding gateways):
     apiVersion: monitoring.coreos.com/v1
     kind: ServiceMonitor
     metadata: { name: apicast, namespace: <gateway namespace> }
     spec:
       selector: { matchLabels: { app: apicast } }
       endpoints: [ { port: metrics, path: /metrics } ]

4. Optional but recommended: APICAST_RESPONSE_CODES=true makes APIcast report
   the response code of each request to 3scale backend, which is what powers the
   per-status-code analytics in the Admin Portal.
`

// ---- raw PromQL escape hatch ----

type promQueryInput struct {
	Query  string `json:"query" jsonschema:"PromQL expression to evaluate, e.g. sum by (status) (increase(upstream_status[1h]))"`
	Range  bool   `json:"range,omitempty" jsonschema:"if true, evaluate over a time range instead of a single instant"`
	Window string `json:"window,omitempty" jsonschema:"for range queries, how far back to go: 15m, 1h, 24h, 7d (optional, default 1h)"`
	Step   string `json:"step,omitempty" jsonschema:"for range queries, the resolution: 30s, 1m, 5m (optional, derived from the window)"`
}

func queryMetrics(ctx context.Context, d *deps, in promQueryInput) (string, error) {
	if !d.prom.enabled() {
		return promDisabledMsg, nil
	}
	if strings.TrimSpace(in.Query) == "" {
		return "", fmt.Errorf("'query' is required")
	}
	dur, window, err := parseWindow(in.Window)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PromQL: %s\n\n", in.Query)

	if !in.Range {
		samples, err := d.prom.instant(ctx, in.Query, time.Time{})
		if err != nil {
			return "", err
		}
		if len(samples) == 0 {
			b.WriteString("(empty result)\n")
			return b.String(), nil
		}
		if len(samples) > maxPromSeries {
			fmt.Fprintf(&b, "(%d series returned, showing the first %d)\n", len(samples), maxPromSeries)
			samples = samples[:maxPromSeries]
		}
		for _, s := range samples {
			fmt.Fprintf(&b, "  %s => %g\n", formatLabels(s.Labels), s.Value)
		}
		return b.String(), nil
	}

	step := dur / 20
	if in.Step != "" {
		sd, _, serr := parseWindow(in.Step)
		if serr != nil {
			return "", fmt.Errorf("invalid step: %w", serr)
		}
		step = sd
	}
	if step < 15*time.Second {
		step = 15 * time.Second
	}
	end := time.Now()
	series, err := d.prom.rangeQuery(ctx, in.Query, end.Add(-dur), end, step)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "range: last %s, step %s, %d series\n\n", window, promDuration(step), len(series))
	if len(series) > 50 {
		fmt.Fprintf(&b, "(showing the first 50 of %d series)\n", len(series))
		series = series[:50]
	}
	for _, s := range series {
		fmt.Fprintf(&b, "  %s\n", formatLabels(s.Labels))
		for _, p := range s.Points {
			fmt.Fprintf(&b, "    %s  %g\n", p.At.UTC().Format(time.RFC3339), p.Value)
		}
	}
	return b.String(), nil
}

func formatLabels(l map[string]string) string {
	if len(l) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, l[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
