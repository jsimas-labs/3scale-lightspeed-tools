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
