package main

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	apiManagerGVR = schema.GroupVersionResource{Group: "apps.3scale.net", Version: "v1alpha1", Resource: "apimanagers"}
	routeGVR      = schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}
)

// k8sClients bundles the typed and dynamic Kubernetes clients used by the tools.
type k8sClients struct {
	clientset        kubernetes.Interface
	dynamic          dynamic.Interface
	defaultNamespace string
}

// newK8sClients builds clients from the in-cluster service account when
// running inside OpenShift, falling back to the local kubeconfig for
// development.
func newK8sClients() (*k8sClients, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, herr := os.UserHomeDir()
			if herr != nil {
				return nil, fmt.Errorf("not in cluster and cannot locate kubeconfig: %w", herr)
			}
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("building kubeconfig from %s: %w", kubeconfig, err)
		}
	}
	cfg.QPS = 20
	cfg.Burst = 40

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	return &k8sClients{clientset: cs, dynamic: dyn}, nil
}

// namespaceOr returns ns if set, otherwise the configured default namespace.
func (k *k8sClients) namespaceOr(ns string) string {
	if ns != "" {
		return ns
	}
	return k.defaultNamespace
}
