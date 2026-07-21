package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Since 3scale 2.16 the Redis databases (backend and system) and, optionally,
// the system RDBMS are external to the cluster: the operator does not deploy
// them and reads connection details from secrets. Troubleshooting therefore
// must start from the secrets, never from assumed in-namespace pods.
//
// Secrets and keys (3scale 2.16):
//   backend-redis:   REDIS_STORAGE_URL, REDIS_QUEUES_URL, *_SENTINEL_HOSTS/ROLE,
//                    CONFIG_REDIS_USERNAME/PASSWORD, CONFIG_QUEUES_USERNAME/PASSWORD, REDIS_SSL_*
//   system-redis:    URL, SENTINEL_HOSTS, SENTINEL_ROLE, REDIS_USERNAME/PASSWORD, REDIS_SSL_*
//   system-database: URL, DB_USER, DB_PASSWORD, DATABASE_SSL_MODE, DB_SSL_*
//   zync:            DATABASE_URL, ZYNC_DATABASE_PASSWORD, DATABASE_SSL_MODE, DB_SSL_*
//
// Passwords are never returned: URLs are redacted and password keys are
// reported only as set/unset.

type dbConfigInput struct {
	Namespace        string `json:"namespace,omitempty" jsonschema:"namespace of the 3scale installation (optional)"`
	TestConnectivity bool   `json:"testConnectivity,omitempty" jsonschema:"if true, attempt a TCP connection to each configured database endpoint (default false)"`
}

type dbEndpoint struct {
	label string
	host  string // host:port
}

func databaseConfig(ctx context.Context, kc *k8sClients, in dbConfigInput) (string, error) {
	ns := kc.namespaceOr(in.Namespace)
	var b strings.Builder
	fmt.Fprintf(&b, "Database configuration for namespace %q (from secrets, credentials redacted):\n", ns)
	fmt.Fprintf(&b, "Note: since 3scale 2.16 backend/system Redis are always external and the system RDBMS may be; the operator reads them from these secrets.\n\n")

	// externalComponents flags from the APIManager CR (best effort).
	if flags := externalComponentFlags(ctx, kc, ns); flags != "" {
		fmt.Fprintf(&b, "APIManager spec.externalComponents: %s\n\n", flags)
	}

	var endpoints []dbEndpoint

	endpoints = append(endpoints, describeSecret(ctx, kc, ns, &b, "backend-redis", []kv{
		{"REDIS_STORAGE_URL", kindURL, "redis"},
		{"REDIS_QUEUES_URL", kindURL, "redis"},
		{"REDIS_STORAGE_SENTINEL_HOSTS", kindHostList, "redis-sentinel"},
		{"REDIS_QUEUES_SENTINEL_HOSTS", kindHostList, "redis-sentinel"},
		{"REDIS_STORAGE_SENTINEL_ROLE", kindPlain, ""},
		{"REDIS_QUEUES_SENTINEL_ROLE", kindPlain, ""},
		{"CONFIG_REDIS_USERNAME", kindPlain, ""},
		{"CONFIG_REDIS_PASSWORD", kindSecret, ""},
		{"CONFIG_QUEUES_USERNAME", kindPlain, ""},
		{"CONFIG_QUEUES_PASSWORD", kindSecret, ""},
		{"REDIS_SSL_CA", kindPresence, ""},
		{"REDIS_SSL_CERT", kindPresence, ""},
		{"REDIS_SSL_KEY", kindPresence, ""},
	})...)

	endpoints = append(endpoints, describeSecret(ctx, kc, ns, &b, "system-redis", []kv{
		{"URL", kindURL, "redis"},
		{"SENTINEL_HOSTS", kindHostList, "redis-sentinel"},
		{"SENTINEL_ROLE", kindPlain, ""},
		{"NAMESPACE", kindPlain, ""},
		{"REDIS_USERNAME", kindPlain, ""},
		{"REDIS_PASSWORD", kindSecret, ""},
		{"REDIS_SSL_CA", kindPresence, ""},
		{"REDIS_SSL_CERT", kindPresence, ""},
		{"REDIS_SSL_KEY", kindPresence, ""},
	})...)

	endpoints = append(endpoints, describeSecret(ctx, kc, ns, &b, "system-database", []kv{
		{"URL", kindURL, "sql"},
		{"DB_USER", kindPlain, ""},
		{"DB_PASSWORD", kindSecret, ""},
		{"DATABASE_SSL_MODE", kindPlain, ""},
		{"DB_SSL_CA", kindPresence, ""},
		{"DB_SSL_CERT", kindPresence, ""},
		{"DB_SSL_KEY", kindPresence, ""},
	})...)

	endpoints = append(endpoints, describeSecret(ctx, kc, ns, &b, "zync", []kv{
		{"DATABASE_URL", kindURL, "sql"},
		{"ZYNC_DATABASE_PASSWORD", kindSecret, ""},
		{"SECRET_KEY_BASE", kindSecret, ""},
		{"DATABASE_SSL_MODE", kindPlain, ""},
		{"DB_SSL_CA", kindPresence, ""},
	})...)

	// Legacy/self-managed in-cluster databases still show up as deployments.
	if names := inClusterDBDeployments(ctx, kc, ns); len(names) > 0 {
		fmt.Fprintf(&b, "\nIn-cluster database deployments found (self-managed or pre-2.16 install): %s\n", strings.Join(names, ", "))
	}

	if in.TestConnectivity && len(endpoints) > 0 {
		b.WriteString("\nTCP connectivity from the MCP server pod (3s timeout):\n")
		b.WriteString(dialEndpoints(ctx, endpoints))
	}
	return b.String(), nil
}

type valueKind int

const (
	kindPlain    valueKind = iota // show value as-is (never a credential)
	kindSecret                    // show only set/unset
	kindURL                       // parse and redact password, collect endpoint
	kindHostList                  // comma-separated host list, collect endpoints
	kindPresence                  // certificates/keys: show only present/absent
)

type kv struct {
	key     string
	kind    valueKind
	dialTag string // label prefix for connectivity checks
}

func describeSecret(ctx context.Context, kc *k8sClients, ns string, b *strings.Builder, name string, keys []kv) []dbEndpoint {
	var endpoints []dbEndpoint
	sec, err := kc.clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			fmt.Fprintf(b, "secret %q: NOT FOUND — component not configured or secret renamed\n", name)
		} else {
			fmt.Fprintf(b, "secret %q: error reading: %v\n", name, err)
		}
		return nil
	}
	fmt.Fprintf(b, "secret %q:\n", name)
	known := map[string]bool{}
	for _, k := range keys {
		known[k.key] = true
		raw, ok := sec.Data[k.key]
		if !ok {
			continue
		}
		val := strings.TrimSpace(string(raw))
		switch k.kind {
		case kindPlain:
			fmt.Fprintf(b, "  %s: %s\n", k.key, val)
		case kindSecret:
			fmt.Fprintf(b, "  %s: [set, redacted]\n", k.key)
		case kindPresence:
			fmt.Fprintf(b, "  %s: [present, %d bytes]\n", k.key, len(raw))
		case kindURL:
			red, hostport := redactURL(val)
			fmt.Fprintf(b, "  %s: %s\n", k.key, red)
			if hostport != "" {
				endpoints = append(endpoints, dbEndpoint{label: name + "/" + k.key, host: hostport})
			}
		case kindHostList:
			fmt.Fprintf(b, "  %s: %s\n", k.key, val)
			for _, h := range strings.Split(val, ",") {
				if hp := hostPortOf(strings.TrimSpace(h), "26379"); hp != "" {
					endpoints = append(endpoints, dbEndpoint{label: name + "/" + k.key, host: hp})
				}
			}
		}
	}
	var extra []string
	for k := range sec.Data {
		if !known[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		fmt.Fprintf(b, "  (other keys present, values not shown: %s)\n", strings.Join(extra, ", "))
	}
	return endpoints
}

// redactURL masks the password in a database URL and returns the redacted URL
// plus the host:port for connectivity checks.
func redactURL(raw string) (redacted, hostport string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[set, unparseable — value redacted]", ""
	}
	if u.User != nil {
		name := u.User.Username()
		if _, has := u.User.Password(); has || name != "" {
			u.User = url.UserPassword(name, "REDACTED")
		}
	}
	defPort := "6379"
	switch u.Scheme {
	case "mysql", "mysql2":
		defPort = "3306"
	case "postgres", "postgresql":
		defPort = "5432"
	}
	red := u.String()
	red = strings.Replace(red, ":REDACTED@", ":[redacted]@", 1)
	return red, hostPortOf(u.Host, defPort)
}

// hostPortOf normalizes "host", "host:port" or "redis://host:port" to host:port.
func hostPortOf(h, defaultPort string) string {
	if h == "" {
		return ""
	}
	if strings.Contains(h, "://") {
		u, err := url.Parse(h)
		if err != nil || u.Host == "" {
			return ""
		}
		h = u.Host
	}
	if _, _, err := net.SplitHostPort(h); err != nil {
		return net.JoinHostPort(h, defaultPort)
	}
	return h
}

func dialEndpoints(ctx context.Context, endpoints []dbEndpoint) string {
	type result struct {
		label, host, status string
	}
	results := make([]result, len(endpoints))
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func(i int, ep dbEndpoint) {
			defer wg.Done()
			d := net.Dialer{Timeout: 3 * time.Second}
			conn, err := d.DialContext(ctx, "tcp", ep.host)
			status := "OK"
			if err != nil {
				status = "FAIL: " + err.Error()
			} else {
				conn.Close()
			}
			results[i] = result{ep.label, ep.host, status}
		}(i, ep)
	}
	wg.Wait()
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "  %s (%s): %s\n", r.label, r.host, r.status)
	}
	return b.String()
}

func externalComponentFlags(ctx context.Context, kc *k8sClients, ns string) string {
	list, err := kc.dynamic.Resource(apiManagerGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil || len(list.Items) == 0 {
		return ""
	}
	ec, ok, _ := unstructured.NestedMap(list.Items[0].Object, "spec", "externalComponents")
	if !ok {
		return "not set (2.16 defaults: backend/system Redis external; zync database operator-provisioned unless declared external)"
	}
	var parts []string
	for _, comp := range []string{"backend", "system", "zync"} {
		if m, ok := ec[comp].(map[string]interface{}); ok {
			for k, v := range m {
				parts = append(parts, fmt.Sprintf("%s.%s=%v", comp, k, v))
			}
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func inClusterDBDeployments(ctx context.Context, kc *k8sClients, ns string) []string {
	deps, err := kc.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	candidates := map[string]bool{
		"backend-redis": true, "system-redis": true, "system-mysql": true,
		"system-postgresql": true, "zync-database": true, "system-memcache": true,
	}
	var found []string
	for _, d := range deps.Items {
		if candidates[d.Name] {
			found = append(found, fmt.Sprintf("%s (%d/%d ready)", d.Name, d.Status.ReadyReplicas, d.Status.Replicas))
		}
	}
	sort.Strings(found)
	return found
}
