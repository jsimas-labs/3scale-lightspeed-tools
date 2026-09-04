package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// APIcast gateways are not confined to the namespace where the APIManager
// lives. Besides the staging/production gateways the 3scale operator deploys
// next to the API Manager, the *APIcast operator* can create self-managed
// gateways (kind APIcast, apps.3scale.net/v1alpha1) in any namespace of the
// cluster, often one namespace per team or per environment. Every metric tool
// therefore discovers gateways cluster-wide first and only then narrows down.

// apicastGateway is one deployed APIcast gateway with the configuration that
// matters for metrics and troubleshooting.
type apicastGateway struct {
	Namespace   string
	Deployment  string
	Source      string // APIManager, APIcast CR (operator) or a bare labelled Deployment
	Environment string // staging / production / unknown
	Image       string
	Desired     int32
	Ready       int32
	Env         map[string]string // selected APICAST_* variables (secret refs described, never resolved)
	MetricsPort bool              // container exposes the 9421 metrics port
	Monitors    []string          // ServiceMonitors/PodMonitors in the namespace
}

func (g apicastGateway) extendedMetrics() bool {
	return strings.EqualFold(g.Env["APICAST_EXTENDED_METRICS"], "true")
}

func (g apicastGateway) responseCodes() bool {
	return strings.EqualFold(g.Env["APICAST_RESPONSE_CODES"], "true")
}

// interestingApicastEnv are the variables reported for each gateway. Values
// that can embed credentials (the portal endpoint carries the access token)
// are redacted by redactEnvValue.
var interestingApicastEnv = []string{
	"APICAST_EXTENDED_METRICS",
	"APICAST_RESPONSE_CODES",
	"APICAST_CONFIGURATION_LOADER",
	"APICAST_CONFIGURATION_CACHE",
	"APICAST_LOG_LEVEL",
	"APICAST_MANAGEMENT_API",
	"APICAST_WORKERS",
	"APICAST_SERVICES_LIST",
	"APICAST_SERVICES_FILTER_BY_URL",
	"APICAST_PATH_ROUTING",
	"THREESCALE_DEPLOYMENT_ENV",
	"THREESCALE_PORTAL_ENDPOINT",
	"BACKEND_ENDPOINT_OVERRIDE",
}

// discoverGateways returns every APIcast gateway in the cluster, or only those
// in ns when ns is not empty.
func discoverGateways(ctx context.Context, kc *k8sClients, ns string) ([]apicastGateway, []string, error) {
	var warnings []string
	if kc == nil || kc.clientset == nil {
		return nil, warnings, fmt.Errorf("no Kubernetes client available")
	}

	// Namespaces that host an APIcast CR, mapped to the CR that owns them.
	crOwners := map[string]string{} // "<ns>/apicast-<name>" -> CR name
	if kc.dynamic == nil {
		warnings = append(warnings, "no dynamic client: APIcast and APIManager custom resources were not consulted")
	} else if list, err := kc.dynamic.Resource(apicastGVR).Namespace(ns).List(ctx, metav1.ListOptions{}); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not list APIcast custom resources (apicast operator may not be installed): %v", err))
	} else {
		for i := range list.Items {
			cr := &list.Items[i]
			crOwners[cr.GetNamespace()+"/apicast-"+cr.GetName()] = cr.GetName()
		}
	}

	// Namespaces that host an APIManager (staging/production gateways).
	amOwners := map[string]string{} // namespace -> APIManager name
	if list, err := listAPIManagers(ctx, kc, ns); err == nil {
		for i := range list.Items {
			amOwners[list.Items[i].GetNamespace()] = list.Items[i].GetName()
		}
	}

	deps, err := apicastDeployments(ctx, kc, ns)
	if err != nil {
		return nil, warnings, err
	}
	monitors := listMonitors(ctx, kc, ns)

	gateways := make([]apicastGateway, 0, len(deps))
	for i := range deps {
		d := &deps[i]
		g := apicastGateway{
			Namespace:  d.Namespace,
			Deployment: d.Name,
			Desired:    1,
			Ready:      d.Status.ReadyReplicas,
			Env:        map[string]string{},
		}
		if d.Spec.Replicas != nil {
			g.Desired = *d.Spec.Replicas
		}
		switch {
		case crOwners[d.Namespace+"/"+d.Name] != "":
			g.Source = "APIcast operator (APIcast/" + crOwners[d.Namespace+"/"+d.Name] + ")"
		case amOwners[d.Namespace] != "":
			g.Source = "APIManager/" + amOwners[d.Namespace]
		default:
			g.Source = "standalone Deployment"
		}
		c := apicastContainer(d)
		if c != nil {
			g.Image = c.Image
			for _, p := range c.Ports {
				if p.ContainerPort == 9421 || p.Name == "metrics" {
					g.MetricsPort = true
				}
			}
			for _, e := range c.Env {
				if contains(interestingApicastEnv, e.Name) {
					g.Env[e.Name] = redactEnvValue(e)
				}
			}
		}
		g.Environment = gatewayEnvironment(d, g.Env)
		g.Monitors = monitors[d.Namespace]
		gateways = append(gateways, g)
	}
	sort.Slice(gateways, func(i, j int) bool {
		if gateways[i].Namespace != gateways[j].Namespace {
			return gateways[i].Namespace < gateways[j].Namespace
		}
		return gateways[i].Deployment < gateways[j].Deployment
	})
	return gateways, warnings, nil
}

// listAPIManagers is a nil-safe wrapper around the dynamic client.
func listAPIManagers(ctx context.Context, kc *k8sClients, ns string) (*unstructured.UnstructuredList, error) {
	if kc.dynamic == nil {
		return nil, fmt.Errorf("no dynamic client")
	}
	return kc.dynamic.Resource(apiManagerGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
}

// apicastDeployments finds APIcast Deployments by label first (both operators
// label their workloads) and falls back to name matching, which catches
// hand-rolled gateways and older releases.
func apicastDeployments(ctx context.Context, kc *k8sClients, ns string) ([]appsv1.Deployment, error) {
	seen := map[string]bool{}
	var out []appsv1.Deployment

	add := func(items []appsv1.Deployment, nameFilter bool) {
		for _, d := range items {
			if nameFilter && !strings.Contains(d.Name, "apicast") {
				continue
			}
			key := d.Namespace + "/" + d.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, d)
		}
	}

	for _, sel := range []string{"threescale_component=apicast", "app=apicast"} {
		list, err := kc.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, fmt.Errorf("listing Deployments with selector %q: %w", sel, err)
		}
		add(list.Items, false)
	}
	// Name-based sweep for gateways without the expected labels.
	all, err := kc.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing Deployments: %w", err)
	}
	add(all.Items, true)
	return out, nil
}

func apicastContainer(d *appsv1.Deployment) *corev1.Container {
	for i := range d.Spec.Template.Spec.Containers {
		c := &d.Spec.Template.Spec.Containers[i]
		if strings.Contains(c.Name, "apicast") || strings.Contains(c.Image, "apicast") {
			return c
		}
	}
	if len(d.Spec.Template.Spec.Containers) > 0 {
		return &d.Spec.Template.Spec.Containers[0]
	}
	return nil
}

func gatewayEnvironment(d *appsv1.Deployment, env map[string]string) string {
	if v := env["THREESCALE_DEPLOYMENT_ENV"]; v != "" && !strings.HasPrefix(v, "[from ") {
		return v
	}
	if v := d.Spec.Template.Labels["threescale_component_element"]; v != "" {
		return v
	}
	switch {
	case strings.Contains(d.Name, "staging"):
		return "staging"
	case strings.Contains(d.Name, "production"):
		return "production"
	}
	return "unknown"
}

// redactEnvValue renders an env var without ever leaking a credential: the
// 3scale portal endpoint embeds the access token in the URL userinfo, and
// values sourced from secrets are described rather than resolved.
func redactEnvValue(e corev1.EnvVar) string {
	if e.ValueFrom != nil {
		switch {
		case e.ValueFrom.SecretKeyRef != nil:
			return fmt.Sprintf("[from secret %s key %s]", e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key)
		case e.ValueFrom.ConfigMapKeyRef != nil:
			return fmt.Sprintf("[from configmap %s key %s]", e.ValueFrom.ConfigMapKeyRef.Name, e.ValueFrom.ConfigMapKeyRef.Key)
		case e.ValueFrom.FieldRef != nil:
			return fmt.Sprintf("[from field %s]", e.ValueFrom.FieldRef.FieldPath)
		}
		return "[from an external reference]"
	}
	if strings.Contains(e.Value, "@") && strings.Contains(e.Value, "://") {
		return redactURLCredentials(e.Value)
	}
	return e.Value
}

// redactURLCredentials strips the whole user-info section of a URL. Unlike the
// database URLs handled by redactURL — where the user name is a harmless
// database account — THREESCALE_PORTAL_ENDPOINT carries the 3scale access
// token *as the user name*, so nothing before the @ may be shown.
func redactURLCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[set, unparseable — value redacted]"
	}
	if u.User == nil {
		return u.String()
	}
	u.User = nil
	return strings.Replace(u.String(), "://", "://[credentials redacted]@", 1)
}

// listMonitors maps namespace -> ServiceMonitor/PodMonitor names, which is how
// user workload monitoring learns to scrape APIcast.
func listMonitors(ctx context.Context, kc *k8sClients, ns string) map[string][]string {
	out := map[string][]string{}
	if kc.dynamic == nil {
		return out
	}
	for kind, gvr := range map[string]schema.GroupVersionResource{"ServiceMonitor": serviceMonitorGVR, "PodMonitor": podMonitorGVR} {
		list, err := kc.dynamic.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range list.Items {
			m := &list.Items[i]
			out[m.GetNamespace()] = append(out[m.GetNamespace()], kind+"/"+m.GetName())
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// discover3scaleNamespaces lists namespaces holding an APIManager, used to
// point the user at the right namespace when the configured default is wrong.
func discover3scaleNamespaces(ctx context.Context, kc *k8sClients) []string {
	if kc == nil || kc.dynamic == nil {
		return nil
	}
	list, err := kc.dynamic.Resource(apiManagerGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for i := range list.Items {
		out = append(out, list.Items[i].GetNamespace())
	}
	sort.Strings(out)
	return dedupe(out)
}

// namespaceHint produces a suffix suggesting the namespaces where 3scale and
// APIcast actually are, so an empty result is never a dead end.
func namespaceHint(ctx context.Context, kc *k8sClients) string {
	var parts []string
	if ns := discover3scaleNamespaces(ctx, kc); len(ns) > 0 {
		parts = append(parts, "APIManager found in: "+strings.Join(ns, ", "))
	}
	if gws, _, err := discoverGateways(ctx, kc, ""); err == nil && len(gws) > 0 {
		var nss []string
		for _, g := range gws {
			nss = append(nss, g.Namespace)
		}
		parts = append(parts, "APIcast gateways found in: "+strings.Join(dedupe(sortedCopy(nss)), ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\nHint: " + strings.Join(parts, "; ") + ". Pass the right value in the \"namespace\" argument."
}

// ---- rendering ----

// renderGatewaysHighlighting reports every gateway in the cluster, noting
// whether a namespace the caller asked about actually holds any. Filtering
// would hide the gateways that matter: in the common topology the APIManager
// namespace has none, and the gateways live in their own namespaces.
func renderGatewaysHighlighting(gws []apicastGateway, warnings []string, asked string) string {
	out := renderGateways(gws, warnings)
	if asked == "" || len(gws) == 0 {
		return out
	}
	var here, elsewhere []string
	for _, g := range gws {
		if g.Namespace == asked {
			here = append(here, g.Deployment)
		} else {
			elsewhere = append(elsewhere, g.Namespace+"/"+g.Deployment)
		}
	}
	var note string
	if len(here) > 0 {
		note = fmt.Sprintf("\nNamespace %q holds: %s.\n", asked, strings.Join(here, ", "))
	} else {
		note = fmt.Sprintf("\nNamespace %q holds no APIcast gateway. This is normal when it is the APIManager namespace: "+
			"self-managed gateways run in their own namespaces. Gateways found elsewhere: %s.\n",
			asked, strings.Join(dedupe(sortedCopy(elsewhere)), ", "))
	}
	return out + note
}

func renderGateways(gws []apicastGateway, warnings []string) string {
	var b strings.Builder
	if len(gws) == 0 {
		b.WriteString("No APIcast gateways found in the cluster.\n\n" +
			"Expected either apicast-staging/apicast-production Deployments next to an APIManager, " +
			"or Deployments named apicast-<name> created by the APIcast operator (kind APIcast, apps.3scale.net/v1alpha1) " +
			"in any namespace. Check that the MCP ServiceAccount can list Deployments cluster-wide.\n")
		for _, w := range warnings {
			fmt.Fprintf(&b, "warning: %s\n", w)
		}
		return b.String()
	}

	nss := map[string]bool{}
	for _, g := range gws {
		nss[g.Namespace] = true
	}
	fmt.Fprintf(&b, "APIcast gateways in the cluster: %d, across %d namespace(s).\n\n", len(gws), len(nss))

	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tDEPLOYMENT\tENV\tREADY\tMANAGED BY\tEXT. METRICS\tRESP. CODES\tMETRICS PORT\tMONITORS")
	for _, g := range gws {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\t%s\t%s\n",
			g.Namespace, g.Deployment, g.Environment, g.Ready, g.Desired, g.Source,
			yesNo(g.extendedMetrics()), yesNo(g.responseCodes()), yesNo(g.MetricsPort),
			orDash(strings.Join(g.Monitors, ",")))
	}
	w.Flush()

	b.WriteString("\nPer-gateway configuration:\n")
	for _, g := range gws {
		fmt.Fprintf(&b, "\n  %s/%s (%s, %s)\n", g.Namespace, g.Deployment, g.Environment, g.Source)
		fmt.Fprintf(&b, "    image: %s\n", orDash(g.Image))
		keys := make([]string, 0, len(g.Env))
		for k := range g.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			b.WriteString("    env: none of the tracked APICAST_* variables are set (defaults in effect)\n")
		}
		for _, k := range keys {
			fmt.Fprintf(&b, "    %s: %s\n", k, g.Env[k])
		}
	}

	var noExt []string
	for _, g := range gws {
		if !g.extendedMetrics() {
			noExt = append(noExt, g.Namespace+"/"+g.Deployment)
		}
	}
	if len(noExt) > 0 {
		fmt.Fprintf(&b, "\nNote: APICAST_EXTENDED_METRICS is not enabled on: %s.\n"+
			"Without it, upstream_status and the response-time histograms carry no service_id/service_system_name labels, "+
			"so per-API analysis is impossible on those gateways — analyze_api_metrics can only report gateway-wide totals.\n",
			strings.Join(noExt, ", "))
	}
	for _, wmsg := range warnings {
		fmt.Fprintf(&b, "\nwarning: %s\n", wmsg)
	}
	return b.String()
}

// ---- small helpers ----

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// dedupe removes consecutive duplicates from an already sorted slice.
func dedupe(sorted []string) []string {
	out := make([]string, 0, len(sorted))
	var last string
	for i, v := range sorted {
		if i == 0 || v != last {
			out = append(out, v)
		}
		last = v
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
