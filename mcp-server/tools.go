package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/duration"
)

const maxLogBytes = 256 * 1024 // cap log payloads returned to the model

type nsInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace of the 3scale installation (optional, defaults to the server's configured namespace)"`
}

type podLogsInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace of the 3scale installation (optional)"`
	Pod       string `json:"pod" jsonschema:"name of the pod to fetch logs from"`
	Container string `json:"container,omitempty" jsonschema:"container name (optional, defaults to the first container)"`
	TailLines int64  `json:"tailLines,omitempty" jsonschema:"number of lines from the end of the log (optional, default 100, max 2000)"`
	Previous  bool   `json:"previous,omitempty" jsonschema:"fetch logs of the previous (crashed) container instance instead of the current one"`
}

type eventsInput struct {
	Namespace    string `json:"namespace,omitempty" jsonschema:"namespace of the 3scale installation (optional)"`
	OnlyWarnings bool   `json:"onlyWarnings,omitempty" jsonschema:"if true, return only Warning events (default false returns all)"`
	Limit        int    `json:"limit,omitempty" jsonschema:"maximum number of events to return (optional, default 30)"`
}

func registerTools(server *mcp.Server, d *deps) {
	kc := d.kc

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_diagnose",
		Description: "One-shot health summary of a 3scale API Management installation: APIManager status conditions, " +
			"deployments that are not ready, pods that are not running/ready (with container failure reasons), " +
			"unbound PersistentVolumeClaims and recent Warning events. Use this first when troubleshooting.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nsInput) (*mcp.CallToolResult, any, error) {
		return textResult(diagnose(ctx, kc, kc.namespaceOr(in.Namespace))), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_get_apimanager_status",
		Description: "Get the APIManager custom resource(s) (apps.3scale.net/v1alpha1) in the namespace: wildcard domain, " +
			"version and status conditions reported by the 3scale operator.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nsInput) (*mcp.CallToolResult, any, error) {
		out, err := apiManagerStatus(ctx, kc, kc.namespaceOr(in.Namespace))
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_list_pods",
		Description: "List all pods in the 3scale namespace with phase, readiness, restart counts, failure reasons and age. " +
			"Covers apicast-staging/production, system-app, system-sidekiq, system-searchd, backend-listener/worker/cron, " +
			"zync, zync-que and memcached. Note: since 3scale 2.16 Redis and (usually) the system database are external " +
			"and have no pods here — use 3scale_check_database_config for those.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nsInput) (*mcp.CallToolResult, any, error) {
		out, err := listPods(ctx, kc, kc.namespaceOr(in.Namespace))
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_get_pod_logs",
		Description: "Fetch recent logs from a pod in the 3scale namespace. Supports selecting the container, tailing N lines " +
			"and reading the previous (crashed) container instance, which is essential for CrashLoopBackOff analysis.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in podLogsInput) (*mcp.CallToolResult, any, error) {
		out, err := podLogs(ctx, kc, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_get_deployments",
		Description: "List Deployments in the 3scale namespace with desired/ready/available replica counts and any " +
			"non-healthy conditions (e.g. ProgressDeadlineExceeded).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nsInput) (*mcp.CallToolResult, any, error) {
		out, err := listDeployments(ctx, kc, kc.namespaceOr(in.Namespace))
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_get_events",
		Description: "List recent Kubernetes events in the 3scale namespace (optionally only Warning events), newest first. " +
			"Useful to spot scheduling failures, image pull errors, probe failures and OOM kills.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in eventsInput) (*mcp.CallToolResult, any, error) {
		out, err := listEvents(ctx, kc, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_check_routes",
		Description: "List OpenShift Routes in the 3scale namespace (admin/master/developer portals, apicast gateways) with " +
			"host, target service, TLS termination and admission status. Missing or not-admitted routes commonly " +
			"indicate zync-que replication problems.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nsInput) (*mcp.CallToolResult, any, error) {
		out, err := listRoutes(ctx, kc, kc.namespaceOr(in.Namespace))
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_check_database_config",
		Description: "Inspect the database configuration of 3scale from its connection secrets (backend-redis, system-redis, " +
			"system-database, zync) with all credentials redacted. Since 3scale 2.16 the backend/system Redis databases " +
			"are always external (not deployed by the operator) and the system RDBMS may be, so troubleshooting database " +
			"issues must start here, not from in-cluster pods. Reports endpoints, sentinel hosts, TLS settings, " +
			"APIManager spec.externalComponents flags and optionally tests TCP connectivity to each endpoint.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dbConfigInput) (*mcp.CallToolResult, any, error) {
		out, err := databaseConfig(ctx, kc, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_check_pvcs",
		Description: "List PersistentVolumeClaims in the 3scale namespace (system-storage and, on installs with " +
			"self-managed in-cluster databases, database volumes) with phase, capacity, access modes and storage class. " +
			"Pending PVCs block the dependent pods from starting.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nsInput) (*mcp.CallToolResult, any, error) {
		out, err := listPVCs(ctx, kc, kc.namespaceOr(in.Namespace))
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	// ---- APIcast topology and metrics ----

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_list_apicast_gateways",
		Description: "Discover every APIcast gateway in the cluster, in ANY namespace: the staging/production gateways " +
			"deployed next to an APIManager and the self-managed gateways created by the APIcast operator (kind APIcast, " +
			"apps.3scale.net/v1alpha1). Reports namespace, readiness, managing operator, image, the APICAST_* settings that " +
			"matter (extended metrics, response codes, configuration cache, log level, portal endpoint with credentials " +
			"redacted) and whether a ServiceMonitor/PodMonitor exists. Always searches the ENTIRE cluster: in a typical install " +
			"the APIManager namespace contains no gateway at all, because self-managed APIcast runs in its own namespaces. " +
			"Start here when you do not know where the gateways are, or when per-API metrics come back empty.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nsInput) (*mcp.CallToolResult, any, error) {
		// Discovery is always cluster-wide; a namespace argument only
		// highlights, because gateways commonly live outside the APIManager
		// namespace and filtering would hide them.
		gws, warns, err := discoverGateways(ctx, d.kc, "")
		if err != nil {
			return nil, nil, err
		}
		return textResult(renderGatewaysHighlighting(gws, warns, in.Namespace)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_list_apis",
		Description: "List the APIs (3scale products) served by APIcast in a time window, ranked by traffic, with request " +
			"count, 4xx, 5xx and error rate for each, plus the namespaces serving them. Product display names come from the " +
			"3scale Admin API when reachable; products configured but idle in the window are listed separately. Use this to " +
			"find which API to investigate, or to confirm that an API is receiving traffic at all. Searches the whole cluster; " +
			"a namespace argument is a hint only and is widened automatically if it matches no traffic.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listAPIsInput) (*mcp.CallToolResult, any, error) {
		out, err := listAPIs(ctx, d, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_analyze_api_metrics",
		Description: "Analyse ONE API from APIcast metrics. Finds the API by product name, system name or numeric service id " +
			"and returns: total requests and rate; the full HTTP status-code breakdown with counts, shares and what each code " +
			"usually means in APIcast; 2xx/4xx/5xx split; traffic and 5xx per gateway (across namespaces); latency (avg/p95/p99) " +
			"for the client-observed total and for the upstream API, isolating APIcast overhead; the calls APIcast makes to " +
			"3scale backend; a request/error timeline; the state of the APIcast pods serving it; and automatic findings that " +
			"name the probable cause. This is the main tool for 'why is my API returning errors / slow'. " +
			"The API is located CLUSTER-WIDE: do not pass a namespace to help it find one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in analyzeAPIInput) (*mcp.CallToolResult, any, error) {
		out, err := analyzeAPI(ctx, d, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_traffic_overview",
		Description: "Cluster-wide APIcast traffic health for a time window: total requests and status-class split, traffic and " +
			"error rate per gateway and namespace, the APIs producing the most 5xx, the calls to 3scale backend, nginx error-log " +
			"volume by level, nginx connection states and shared dictionaries close to full. Use it when the question is about " +
			"the gateway fleet rather than one API.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in overviewInput) (*mcp.CallToolResult, any, error) {
		out, err := trafficOverview(ctx, d, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_check_metrics_pipeline",
		Description: "Explain why APIcast metrics are missing or incomplete. Checks the gateways found in every namespace and " +
			"their APICAST_EXTENDED_METRICS setting, the presence of ServiceMonitors/PodMonitors, whether OpenShift user " +
			"workload monitoring is enabled, whether Prometheus/Thanos answers and which APIcast metrics and per-API labels " +
			"exist, plus scrape target health and 3scale Admin API reachability. Returns the exact configuration to apply. " +
			"Call this whenever a metrics tool returns no data.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nsInput) (*mcp.CallToolResult, any, error) {
		out, err := checkMetricsPipeline(ctx, d, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "3scale_query_metrics",
		Description: "Run an arbitrary read-only PromQL query against the cluster monitoring stack, as an instant or range " +
			"query. Use it only when the dedicated tools do not cover the question. Useful APIcast metrics: upstream_status " +
			"(counter, labels status/service_id/service_system_name), total_response_time_seconds and " +
			"upstream_response_time_seconds (histograms), threescale_backend_calls (labels endpoint/status), " +
			"nginx_http_connections, nginx_error_log, openresty_shdict_free_space and openresty_shdict_capacity.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in promQueryInput) (*mcp.CallToolResult, any, error) {
		out, err := queryMetrics(ctx, d, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func age(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(t.Time))
}

// ---- pods ----

func listPods(ctx context.Context, kc *k8sClients, ns string) (string, error) {
	pods, err := kc.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing pods in %q: %w", ns, err)
	}
	if len(pods.Items) == 0 {
		return fmt.Sprintf("No pods found in namespace %q. Verify that 3scale is installed in this namespace.%s", ns, namespaceHint(ctx, kc)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Pods in namespace %q (%d):\n\n", ns, len(pods.Items))
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPHASE\tREADY\tRESTARTS\tAGE\tISSUES")
	for _, p := range pods.Items {
		ready, total, restarts, issues := podContainerSummary(&p)
		issueStr := "-"
		if len(issues) > 0 {
			issueStr = strings.Join(issues, "; ")
		}
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%d\t%s\t%s\n",
			p.Name, string(p.Status.Phase), ready, total, restarts, age(p.CreationTimestamp), issueStr)
	}
	w.Flush()
	return b.String(), nil
}

// podContainerSummary returns readiness counts, total restarts and a list of
// human-readable issues (waiting/terminated reasons, probe-visible states).
func podContainerSummary(p *corev1.Pod) (ready, total int, restarts int32, issues []string) {
	total = len(p.Spec.Containers)
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			msg := cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				msg += ": " + firstLine(cs.State.Waiting.Message)
			}
			issues = append(issues, fmt.Sprintf("%s %s", cs.Name, msg))
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && cs.State.Terminated.Reason != "Completed" {
			issues = append(issues, fmt.Sprintf("%s terminated (%s, exit %d)", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode))
		}
		if cs.LastTerminationState.Terminated != nil && cs.RestartCount > 0 {
			lt := cs.LastTerminationState.Terminated
			issues = append(issues, fmt.Sprintf("%s last restart: %s (exit %d)", cs.Name, lt.Reason, lt.ExitCode))
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			issues = append(issues, "unschedulable: "+firstLine(c.Message))
		}
	}
	return ready, total, restarts, issues
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- logs ----

func podLogs(ctx context.Context, kc *k8sClients, in podLogsInput) (string, error) {
	if in.Pod == "" {
		return "", fmt.Errorf("'pod' is required")
	}
	ns := kc.namespaceOr(in.Namespace)
	tail := in.TailLines
	if tail <= 0 {
		tail = 100
	}
	if tail > 2000 {
		tail = 2000
	}
	opts := &corev1.PodLogOptions{
		Container: in.Container,
		TailLines: &tail,
		Previous:  in.Previous,
	}
	stream, err := kc.clientset.CoreV1().Pods(ns).GetLogs(in.Pod, opts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("fetching logs for pod %s/%s: %w", ns, in.Pod, err)
	}
	defer stream.Close()

	data, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil {
		return "", fmt.Errorf("reading log stream: %w", err)
	}
	header := fmt.Sprintf("Logs for pod %s/%s", ns, in.Pod)
	if in.Container != "" {
		header += " container " + in.Container
	}
	if in.Previous {
		header += " (previous instance)"
	}
	body := string(data)
	if body == "" {
		body = "<empty log>"
	}
	if len(data) == maxLogBytes {
		body += "\n... [truncated at 256KiB]"
	}
	return fmt.Sprintf("%s (last %d lines):\n\n%s", header, tail, body), nil
}

// ---- deployments ----

func listDeployments(ctx context.Context, kc *k8sClients, ns string) (string, error) {
	deps, err := kc.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing deployments in %q: %w", ns, err)
	}
	if len(deps.Items) == 0 {
		return fmt.Sprintf("No Deployments found in namespace %q. Note: 3scale 2.14 and earlier used DeploymentConfigs; from 2.15 on all components run as Deployments.", ns), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Deployments in namespace %q (%d):\n\n", ns, len(deps.Items))
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESIRED\tREADY\tAVAILABLE\tUPDATED\tAGE\tCONDITIONS")
	for _, d := range deps.Items {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		var bad []string
		for _, c := range d.Status.Conditions {
			healthy := (c.Type == "Available" && c.Status == corev1.ConditionTrue) ||
				(c.Type == "Progressing" && c.Status == corev1.ConditionTrue && c.Reason != "ProgressDeadlineExceeded")
			if !healthy {
				bad = append(bad, fmt.Sprintf("%s=%s (%s)", c.Type, c.Status, c.Reason))
			}
		}
		condStr := "healthy"
		if len(bad) > 0 {
			condStr = strings.Join(bad, "; ")
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			d.Name, desired, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas,
			age(d.CreationTimestamp), condStr)
	}
	w.Flush()
	return b.String(), nil
}

// ---- events ----

func listEvents(ctx context.Context, kc *k8sClients, in eventsInput) (string, error) {
	ns := kc.namespaceOr(in.Namespace)
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	evs, err := kc.clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing events in %q: %w", ns, err)
	}
	items := evs.Items
	if in.OnlyWarnings {
		filtered := items[:0]
		for _, e := range items {
			if e.Type == corev1.EventTypeWarning {
				filtered = append(filtered, e)
			}
		}
		items = filtered
	}
	sort.Slice(items, func(i, j int) bool {
		return eventTime(&items[i]).After(eventTime(&items[j]))
	})
	if len(items) > limit {
		items = items[:limit]
	}
	if len(items) == 0 {
		return fmt.Sprintf("No matching events in namespace %q.", ns), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Events in namespace %q (newest first, %d shown):\n\n", ns, len(items))
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LAST SEEN\tTYPE\tREASON\tOBJECT\tCOUNT\tMESSAGE")
	for i := range items {
		e := &items[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s/%s\t%d\t%s\n",
			age(metav1.Time{Time: eventTime(e)}), e.Type, e.Reason,
			strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name,
			max32(e.Count, 1), firstLine(e.Message))
	}
	w.Flush()
	return b.String(), nil
}

func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// ---- APIManager CR ----

func apiManagerStatus(ctx context.Context, kc *k8sClients, ns string) (string, error) {
	list, err := kc.dynamic.Resource(apiManagerGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing APIManager resources in %q (is the 3scale operator installed?): %w", ns, err)
	}
	if len(list.Items) == 0 {
		return fmt.Sprintf("No APIManager custom resource found in namespace %q. 3scale may not be installed here, or it was installed without the operator. "+
			"Self-managed APIcast gateways can also exist without an APIManager — use 3scale_list_apicast_gateways.%s", ns, namespaceHint(ctx, kc)), nil
	}
	var b strings.Builder
	for i := range list.Items {
		am := &list.Items[i]
		fmt.Fprintf(&b, "APIManager %q (namespace %s):\n", am.GetName(), ns)
		if wd, ok, _ := unstructured.NestedString(am.Object, "spec", "wildcardDomain"); ok {
			fmt.Fprintf(&b, "  wildcardDomain: %s\n", wd)
		}
		if v, ok := am.GetAnnotations()["apps.3scale.net/apimanager-threescale-version"]; ok {
			fmt.Fprintf(&b, "  3scale version: %s\n", v)
		}
		conds, ok, _ := unstructured.NestedSlice(am.Object, "status", "conditions")
		if !ok || len(conds) == 0 {
			b.WriteString("  status: no conditions reported yet (operator may still be reconciling)\n")
			continue
		}
		b.WriteString("  conditions:\n")
		for _, c := range conds {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			line := fmt.Sprintf("    - %v=%v", cm["type"], cm["status"])
			if r, ok := cm["reason"].(string); ok && r != "" {
				line += " reason=" + r
			}
			if m, ok := cm["message"].(string); ok && m != "" {
				line += " message=" + firstLine(m)
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String(), nil
}

// ---- routes ----

func listRoutes(ctx context.Context, kc *k8sClients, ns string) (string, error) {
	list, err := kc.dynamic.Resource(routeGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing routes in %q: %w", ns, err)
	}
	if len(list.Items) == 0 {
		return fmt.Sprintf("No Routes found in namespace %q. For a working 3scale install expect routes for system-provider (admin portal), system-master, system-developer and apicast staging/production. Missing routes usually point at zync-que issues — check zync pods and consider forcing route recreation.", ns), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Routes in namespace %q (%d):\n\n", ns, len(list.Items))
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHOST\tSERVICE\tTLS\tADMITTED")
	for i := range list.Items {
		r := &list.Items[i]
		host, _, _ := unstructured.NestedString(r.Object, "spec", "host")
		svc, _, _ := unstructured.NestedString(r.Object, "spec", "to", "name")
		tls, ok, _ := unstructured.NestedString(r.Object, "spec", "tls", "termination")
		if !ok {
			tls = "none"
		}
		admitted := "Unknown"
		ingresses, _, _ := unstructured.NestedSlice(r.Object, "status", "ingress")
		for _, ing := range ingresses {
			im, ok := ing.(map[string]interface{})
			if !ok {
				continue
			}
			conds, _, _ := unstructured.NestedSlice(im, "conditions")
			for _, c := range conds {
				cm, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				if cm["type"] == "Admitted" {
					admitted = fmt.Sprintf("%v", cm["status"])
					if reason, ok := cm["reason"].(string); ok && reason != "" && admitted != "True" {
						admitted += " (" + reason + ")"
					}
				}
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.GetName(), host, svc, tls, admitted)
	}
	w.Flush()
	return b.String(), nil
}

// ---- PVCs ----

func listPVCs(ctx context.Context, kc *k8sClients, ns string) (string, error) {
	pvcs, err := kc.clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing PVCs in %q: %w", ns, err)
	}
	if len(pvcs.Items) == 0 {
		return fmt.Sprintf("No PersistentVolumeClaims found in namespace %q.", ns), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PersistentVolumeClaims in namespace %q (%d):\n\n", ns, len(pvcs.Items))
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPHASE\tCAPACITY\tACCESS MODES\tSTORAGECLASS\tAGE")
	for _, p := range pvcs.Items {
		capacity := "-"
		if q, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		modes := make([]string, 0, len(p.Spec.AccessModes))
		for _, m := range p.Spec.AccessModes {
			modes = append(modes, string(m))
		}
		sc := "-"
		if p.Spec.StorageClassName != nil {
			sc = *p.Spec.StorageClassName
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Name, string(p.Status.Phase), capacity, strings.Join(modes, ","), sc, age(p.CreationTimestamp))
	}
	w.Flush()
	return b.String(), nil
}

// ---- aggregate diagnosis ----

func diagnose(ctx context.Context, kc *k8sClients, ns string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== 3scale health summary for namespace %q ===\n\n", ns)
	problems := 0

	// APIManager
	b.WriteString("--- APIManager ---\n")
	if out, err := apiManagerStatus(ctx, kc, ns); err != nil {
		problems++
		fmt.Fprintf(&b, "error: %v\n", err)
	} else {
		b.WriteString(out)
		if strings.Contains(out, "status=False") || strings.Contains(out, "No APIManager") {
			problems++
		}
	}

	// Deployments not ready
	b.WriteString("\n--- Deployments not fully ready ---\n")
	deps, err := kc.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	switch {
	case err != nil:
		problems++
		fmt.Fprintf(&b, "error listing deployments: %v\n", err)
	default:
		notReady := 0
		for _, d := range deps.Items {
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			if d.Status.ReadyReplicas < desired {
				notReady++
				fmt.Fprintf(&b, "  %s: %d/%d ready\n", d.Name, d.Status.ReadyReplicas, desired)
			}
		}
		if notReady == 0 {
			fmt.Fprintf(&b, "  all %d deployments ready\n", len(deps.Items))
		}
		problems += notReady
	}

	// Pods with issues
	b.WriteString("\n--- Pods with issues ---\n")
	pods, err := kc.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	switch {
	case err != nil:
		problems++
		fmt.Fprintf(&b, "error listing pods: %v\n", err)
	default:
		bad := 0
		for i := range pods.Items {
			p := &pods.Items[i]
			ready, total, restarts, issues := podContainerSummary(p)
			healthy := p.Status.Phase == corev1.PodRunning && ready == total
			if p.Status.Phase == corev1.PodSucceeded {
				healthy = true // completed jobs/hooks are fine
			}
			if healthy && len(issues) == 0 {
				continue
			}
			bad++
			fmt.Fprintf(&b, "  %s: phase=%s ready=%d/%d restarts=%d", p.Name, p.Status.Phase, ready, total, restarts)
			if len(issues) > 0 {
				fmt.Fprintf(&b, " issues=[%s]", strings.Join(issues, "; "))
			}
			b.WriteString("\n")
		}
		if bad == 0 {
			fmt.Fprintf(&b, "  all %d pods healthy\n", len(pods.Items))
		}
		problems += bad
	}

	// PVCs not bound
	b.WriteString("\n--- PVCs not Bound ---\n")
	pvcs, err := kc.clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	switch {
	case err != nil:
		problems++
		fmt.Fprintf(&b, "error listing PVCs: %v\n", err)
	default:
		unbound := 0
		for _, p := range pvcs.Items {
			if p.Status.Phase != corev1.ClaimBound {
				unbound++
				fmt.Fprintf(&b, "  %s: %s\n", p.Name, p.Status.Phase)
			}
		}
		if unbound == 0 {
			fmt.Fprintf(&b, "  all %d PVCs bound\n", len(pvcs.Items))
		}
		problems += unbound
	}

	// Database configuration and reachability (external since 3scale 2.16)
	b.WriteString("\n--- Database configuration (external since 2.16; credentials redacted) ---\n")
	if out, err := databaseConfig(ctx, kc, dbConfigInput{Namespace: ns, TestConnectivity: true}); err != nil {
		problems++
		fmt.Fprintf(&b, "error reading database secrets: %v\n", err)
	} else {
		b.WriteString(out)
		if strings.Contains(out, "FAIL:") || strings.Contains(out, "NOT FOUND") {
			problems++
		}
	}

	// Recent warning events
	b.WriteString("\n--- Recent Warning events (last 10) ---\n")
	if out, err := listEvents(ctx, kc, eventsInput{Namespace: ns, OnlyWarnings: true, Limit: 10}); err != nil {
		fmt.Fprintf(&b, "error listing events: %v\n", err)
	} else {
		b.WriteString(out)
	}

	b.WriteString("\n=== Verdict: ")
	if problems == 0 {
		b.WriteString("no problems detected. The 3scale installation looks healthy. ===\n")
	} else {
		fmt.Fprintf(&b, "%d potential problem(s) detected. Drill down with 3scale_get_pod_logs, 3scale_get_events and 3scale_check_routes. ===\n", problems)
	}
	return b.String()
}
