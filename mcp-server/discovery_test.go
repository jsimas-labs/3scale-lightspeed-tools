package main

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func apicastDeployment(ns, name string, labels map[string]string, env []corev1.EnvVar) *appsv1.Deployment {
	replicas := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "apicast",
					Image: "registry.redhat.io/3scale-amp2/apicast-gateway-rhel9:latest",
					Env:   env,
					Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9421}},
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2},
	}
}

// The APIcast operator can place gateways in namespaces that have no
// APIManager at all, so discovery must sweep the whole cluster.
func TestDiscoverGatewaysAcrossNamespaces(t *testing.T) {
	cs := fake.NewSimpleClientset(
		apicastDeployment("3scale", "apicast-production",
			map[string]string{"threescale_component": "apicast", "threescale_component_element": "production"},
			[]corev1.EnvVar{{Name: "APICAST_EXTENDED_METRICS", Value: "true"}}),
		apicastDeployment("3scale", "apicast-staging",
			map[string]string{"threescale_component": "apicast", "threescale_component_element": "staging"}, nil),
		apicastDeployment("team-a", "apicast-selfmanaged",
			map[string]string{"app": "apicast"},
			[]corev1.EnvVar{{Name: "THREESCALE_DEPLOYMENT_ENV", Value: "production"}}),
		// An unrelated workload must not be picked up.
		apicastDeployment("other", "web-frontend", map[string]string{"app": "web"}, nil),
	)
	kc := &k8sClients{clientset: cs, defaultNamespace: "3scale"}

	gws, _, err := discoverGateways(context.Background(), kc, "")
	if err != nil {
		t.Fatalf("discoverGateways: %v", err)
	}
	if len(gws) != 3 {
		var names []string
		for _, g := range gws {
			names = append(names, g.Namespace+"/"+g.Deployment)
		}
		t.Fatalf("got %d gateways (%s), want 3", len(gws), strings.Join(names, ", "))
	}
	byName := map[string]apicastGateway{}
	for _, g := range gws {
		byName[g.Namespace+"/"+g.Deployment] = g
	}
	if _, ok := byName["team-a/apicast-selfmanaged"]; !ok {
		t.Error("a gateway in a namespace without an APIManager must still be found")
	}
	if !byName["3scale/apicast-production"].extendedMetrics() {
		t.Error("APICAST_EXTENDED_METRICS=true was not detected")
	}
	if byName["3scale/apicast-staging"].extendedMetrics() {
		t.Error("staging has no extended metrics and must not be reported as having them")
	}
	if got := byName["3scale/apicast-staging"].Environment; got != "staging" {
		t.Errorf("environment from labels = %q, want staging", got)
	}
	if got := byName["team-a/apicast-selfmanaged"].Environment; got != "production" {
		t.Errorf("environment from THREESCALE_DEPLOYMENT_ENV = %q, want production", got)
	}
	if !byName["team-a/apicast-selfmanaged"].MetricsPort {
		t.Error("the 9421 metrics port should be detected")
	}
	if got := byName["3scale/apicast-production"].Ready; got != 2 {
		t.Errorf("ready replicas = %d, want 2", got)
	}
}

func TestDiscoverGatewaysScopedToNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(
		apicastDeployment("3scale", "apicast-production", map[string]string{"app": "apicast"}, nil),
		apicastDeployment("team-a", "apicast-selfmanaged", map[string]string{"app": "apicast"}, nil),
	)
	kc := &k8sClients{clientset: cs, defaultNamespace: "3scale"}
	gws, _, err := discoverGateways(context.Background(), kc, "team-a")
	if err != nil {
		t.Fatalf("discoverGateways: %v", err)
	}
	if len(gws) != 1 || gws[0].Namespace != "team-a" {
		t.Fatalf("namespace scoping failed: %+v", gws)
	}
}

func TestDiscoverGatewaysDeduplicates(t *testing.T) {
	// A Deployment carrying both selectors, and a matching name, must appear once.
	cs := fake.NewSimpleClientset(apicastDeployment("3scale", "apicast-production",
		map[string]string{"app": "apicast", "threescale_component": "apicast"}, nil))
	kc := &k8sClients{clientset: cs}
	gws, _, err := discoverGateways(context.Background(), kc, "")
	if err != nil {
		t.Fatalf("discoverGateways: %v", err)
	}
	if len(gws) != 1 {
		t.Fatalf("got %d gateways, want 1", len(gws))
	}
}

// An API is a 3scale product served by whichever gateways are configured for
// it, so metric lookups must never be narrowed by namespace unless the caller
// asks: filtering by THREESCALE_NAMESPACE hid APIs that plainly existed.
func TestResolveScopeIsClusterWideByDefault(t *testing.T) {
	cs := fake.NewSimpleClientset(
		apicastDeployment("team-a", "apicast-team-a", map[string]string{"app": "apicast"}, nil))
	d := &deps{kc: &k8sClients{clientset: cs, defaultNamespace: "my-3scale"}}

	sc := resolveScope(context.Background(), d, "", "")
	if len(sc.Namespaces) != 0 {
		t.Fatalf("scope = %v, want no namespace filter", sc.Namespaces)
	}
	if sc.matchers() != "" {
		t.Errorf("no namespace matcher may be emitted, got %q", sc.matchers())
	}
	if !strings.Contains(sc.describe(), "cluster-wide") {
		t.Errorf("the report must state the scope, got %q", sc.describe())
	}
}

// THREESCALE_NAMESPACE still has a job: it identifies the API Manager whose
// Admin Portal supplies product display names.
func TestConfiguredNamespaceDrivesTheProductCatalogNotTheMetricFilter(t *testing.T) {
	d := &deps{
		kc:    &k8sClients{clientset: fake.NewSimpleClientset(), defaultNamespace: "my-3scale"},
		admin: newAdminClient(nil, "", "", false, true),
	}
	if got := adminNamespace(context.Background(), d, ""); got != "my-3scale" {
		t.Errorf("the Admin API lookup must use THREESCALE_NAMESPACE, got %q", got)
	}
	if got := adminNamespace(context.Background(), d, "other"); got != "other" {
		t.Errorf("an explicit namespace must win, got %q", got)
	}
}

func TestResolveScopeExplicitArgumentWins(t *testing.T) {
	cs := fake.NewSimpleClientset(
		apicastDeployment("team-a", "apicast-team-a", map[string]string{"app": "apicast"}, nil))
	d := &deps{kc: &k8sClients{clientset: cs, defaultNamespace: "my-3scale"}}

	sc := resolveScope(context.Background(), d, "team-a", "apicast-team-a")
	if len(sc.Namespaces) != 1 || sc.Namespaces[0] != "team-a" {
		t.Fatalf("an explicit namespace must win, got %v", sc.Namespaces)
	}
	if !strings.Contains(sc.matchers(), `pod=~"apicast-team-a-.*"`) {
		t.Errorf("the gateway filter was lost: %q", sc.matchers())
	}
}

// Only when nothing is configured and nothing is discovered may the query be
// unscoped — and the result has to admit it.
func TestResolveScopeFallsBackToClusterWide(t *testing.T) {
	d := &deps{kc: &k8sClients{clientset: fake.NewSimpleClientset(), defaultNamespace: ""}}
	sc := resolveScope(context.Background(), d, "", "")
	if len(sc.Namespaces) != 0 {
		t.Fatalf("expected an unscoped query, got %v", sc.Namespaces)
	}
	if sc.matchers() != "" {
		t.Errorf("an unscoped query must produce no namespace matcher, got %q", sc.matchers())
	}
	if !strings.Contains(sc.describe(), "cluster-wide") {
		t.Errorf("the fallback must be stated in the output, got %q", sc.describe())
	}
}
