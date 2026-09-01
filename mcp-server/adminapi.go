package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The Prometheus labels APIcast attaches to per-service metrics are the
// service *id* and the service *system name*. The name an operator sees in the
// Admin Portal ("Echo API") is neither of those, so resolving "analyse the
// Echo API" needs the product catalog from the 3scale Account Management API.
//
// This client is strictly optional and best-effort: it reads the default
// tenant's admin route (system-provider) and its ADMIN_ACCESS_TOKEN from the
// system-seed secret. When the route, the secret or the RBAC is missing, the
// metric tools fall back to matching against the labels present in Prometheus
// and say so instead of failing.

const adminCatalogTTL = 5 * time.Minute

// apiService is one 3scale product (service) as the Admin Portal knows it.
type apiService struct {
	ID             string `json:"-"`
	Name           string `json:"name"`
	SystemName     string `json:"system_name"`
	State          string `json:"state"`
	BackendVersion string `json:"backend_version"`
	Description    string `json:"description"`
}

type adminClient struct {
	kc       *k8sClients
	baseURL  string // explicit override; otherwise discovered from the route
	token    string // explicit override; otherwise read from system-seed
	disabled bool
	http     *http.Client

	mu    sync.Mutex
	cache map[string]adminCatalog // keyed by namespace
}

type adminCatalog struct {
	services []apiService
	at       time.Time
	err      error
	endpoint string
}

func newAdminClient(kc *k8sClients, baseURL, token string, insecure, disabled bool) *adminClient {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12} // #nosec G402 -- InsecureSkipVerify is opt-in
	if insecure {
		tlsCfg.InsecureSkipVerify = true
	} else if pool := caPool(); pool != nil {
		tlsCfg.RootCAs = pool
	}
	return &adminClient{
		kc:       kc,
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		token:    token,
		disabled: disabled,
		cache:    map[string]adminCatalog{},
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}
}

// services returns the product catalog of the default tenant in ns. The second
// value is the admin endpoint used, for reporting. Errors are informative
// rather than fatal: callers degrade to label-only matching.
func (a *adminClient) services(ctx context.Context, ns string) ([]apiService, string, error) {
	if a == nil || a.disabled {
		return nil, "", fmt.Errorf("the 3scale Admin API lookup is disabled (-disable-admin-api); API names can only be matched against Prometheus labels")
	}
	a.mu.Lock()
	if c, ok := a.cache[ns]; ok && time.Since(c.at) < adminCatalogTTL {
		a.mu.Unlock()
		return c.services, c.endpoint, c.err
	}
	a.mu.Unlock()

	svcs, endpoint, err := a.fetchServices(ctx, ns)

	a.mu.Lock()
	a.cache[ns] = adminCatalog{services: svcs, at: time.Now(), err: err, endpoint: endpoint}
	a.mu.Unlock()
	return svcs, endpoint, err
}

func (a *adminClient) fetchServices(ctx context.Context, ns string) ([]apiService, string, error) {
	base, err := a.endpointFor(ctx, ns)
	if err != nil {
		return nil, "", err
	}
	token, err := a.tokenFor(ctx, ns)
	if err != nil {
		return nil, base, err
	}

	var all []apiService
	for page := 1; page <= 20; page++ {
		body, err := a.get(ctx, base, "/admin/api/services.json", token, url.Values{
			"page":     {strconv.Itoa(page)},
			"per_page": {"200"},
		})
		if err != nil {
			return nil, base, err
		}
		var payload struct {
			Services []struct {
				Service struct {
					ID json.Number `json:"id"`
					apiService
				} `json:"service"`
			} `json:"services"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, base, fmt.Errorf("decoding services.json from %s: %w", base, err)
		}
		if len(payload.Services) == 0 {
			break
		}
		for _, s := range payload.Services {
			svc := s.Service.apiService
			svc.ID = s.Service.ID.String()
			all = append(all, svc)
		}
		if len(payload.Services) < 200 {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, base, nil
}

// proxyConfig returns the integration settings of one product: public
// endpoints, private base URL and credential placement — the fields needed to
// explain 404s (mapping/host) and 502s (upstream). Credentials are redacted.
func (a *adminClient) proxyConfig(ctx context.Context, ns, serviceID string) (map[string]string, error) {
	if a == nil || a.disabled {
		return nil, fmt.Errorf("admin API lookup disabled")
	}
	base, err := a.endpointFor(ctx, ns)
	if err != nil {
		return nil, err
	}
	token, err := a.tokenFor(ctx, ns)
	if err != nil {
		return nil, err
	}
	body, err := a.get(ctx, base, "/admin/api/services/"+url.PathEscape(serviceID)+"/proxy.json", token, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Proxy map[string]any `json:"proxy"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decoding proxy.json: %w", err)
	}
	interesting := []string{
		"endpoint", "sandbox_endpoint", "api_backend", "deployed_at",
		"credentials_location", "auth_app_key", "auth_app_id", "auth_user_key",
		"error_status_auth_failed", "error_status_auth_missing", "error_status_no_match",
		"hostname_rewrite", "oidc_issuer_endpoint", "oidc_issuer_type",
	}
	out := map[string]string{}
	for _, k := range interesting {
		v, ok := payload.Proxy[k]
		if !ok || v == nil {
			continue
		}
		s := fmt.Sprintf("%v", v)
		if k == "oidc_issuer_endpoint" {
			s, _ = redactURL(s)
		}
		out[k] = s
	}
	return out, nil
}

func (a *adminClient) get(ctx context.Context, base, path, token string, params url.Values) ([]byte, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("access_token", token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling the 3scale Admin API at %s: %w (is the admin route reachable from this pod, and does egress allow it?)", base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("the 3scale Admin API rejected the token (%s): the ADMIN_ACCESS_TOKEN in the system-seed secret may lack the required scopes "+
			"(needs the Account Management API scope with read permission)", resp.Status)
	default:
		return nil, fmt.Errorf("the 3scale Admin API returned %s: %s", resp.Status, truncate(strings.TrimSpace(string(body)), 200))
	}
}

// endpointFor resolves the admin portal base URL from the system-provider
// route in the namespace.
func (a *adminClient) endpointFor(ctx context.Context, ns string) (string, error) {
	if a.baseURL != "" {
		return a.baseURL, nil
	}
	list, err := a.kc.dynamic.Resource(routeGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing routes in %q to find the admin portal: %w", ns, err)
	}
	var fallback string
	for i := range list.Items {
		r := &list.Items[i]
		host, _, _ := unstructured.NestedString(r.Object, "spec", "host")
		to, _, _ := unstructured.NestedString(r.Object, "spec", "to", "name")
		if host == "" {
			continue
		}
		if to == "system-provider" || strings.HasPrefix(r.GetName(), "system-provider") {
			return "https://" + host, nil
		}
		if to == "system-master" && fallback == "" {
			fallback = "https://" + host
		}
	}
	if fallback != "" {
		return "", fmt.Errorf("no system-provider (admin portal) route found in %q; only the master route %s is present, "+
			"which cannot list a tenant's products", ns, fallback)
	}
	return "", fmt.Errorf("no admin portal route found in namespace %q; set -admin-url/THREESCALE_ADMIN_URL to point at it", ns)
}

// tokenFor reads the default tenant admin access token from system-seed.
func (a *adminClient) tokenFor(ctx context.Context, ns string) (string, error) {
	if a.token != "" {
		return a.token, nil
	}
	sec, err := a.kc.clientset.CoreV1().Secrets(ns).Get(ctx, "system-seed", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the system-seed secret in %q for ADMIN_ACCESS_TOKEN: %w "+
			"(grant get on this secret, or pass a token via THREESCALE_ACCESS_TOKEN)", ns, err)
	}
	for _, k := range []string{"ADMIN_ACCESS_TOKEN", "MASTER_ACCESS_TOKEN"} {
		if v, ok := sec.Data[k]; ok && len(v) > 0 {
			return strings.TrimSpace(string(v)), nil
		}
	}
	return "", fmt.Errorf("the system-seed secret in %q has no ADMIN_ACCESS_TOKEN key", ns)
}

// matchServices returns the catalog entries matching a free-form query against
// id, system name or display name, most specific match first.
func matchServices(svcs []apiService, query string) []apiService {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var exact, prefix, sub []apiService
	for _, s := range svcs {
		id, name, sys := strings.ToLower(s.ID), strings.ToLower(s.Name), strings.ToLower(s.SystemName)
		switch {
		case id == q || sys == q || name == q:
			exact = append(exact, s)
		case strings.HasPrefix(sys, q) || strings.HasPrefix(name, q):
			prefix = append(prefix, s)
		case strings.Contains(sys, q) || strings.Contains(name, q):
			sub = append(sub, s)
		}
	}
	return append(append(exact, prefix...), sub...)
}
