package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminClientListsAndPaginatesServices(t *testing.T) {
	var gotTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTokens = append(gotTokens, r.URL.Query().Get("access_token"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, `{"services":[
				{"service":{"id":2,"name":"Echo API","system_name":"echo_api","state":"incomplete"}},
				{"service":{"id":3,"name":"Payments","system_name":"payments","state":"incomplete"}}
			]}`)
			return
		}
		fmt.Fprint(w, `{"services":[]}`)
	}))
	defer srv.Close()

	a := newAdminClient(nil, srv.URL, "tok-123", false, false)
	svcs, endpoint, err := a.services(context.Background(), "3scale")
	if err != nil {
		t.Fatalf("services: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("got %d services, want 2", len(svcs))
	}
	if svcs[0].ID != "2" || svcs[0].Name != "Echo API" || svcs[0].SystemName != "echo_api" {
		t.Errorf("unexpected service: %+v", svcs[0])
	}
	if endpoint != srv.URL {
		t.Errorf("endpoint = %q", endpoint)
	}
	if gotTokens[0] != "tok-123" {
		t.Errorf("the access token was not sent: %v", gotTokens)
	}

	// A second call must be served from the cache.
	before := len(gotTokens)
	if _, _, err := a.services(context.Background(), "3scale"); err != nil {
		t.Fatal(err)
	}
	if len(gotTokens) != before {
		t.Errorf("the catalog should be cached, %d extra requests were made", len(gotTokens)-before)
	}
}

func TestAdminClientExplainsRejectedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	a := newAdminClient(nil, srv.URL, "bad", false, false)
	_, _, err := a.services(context.Background(), "3scale")
	if err == nil || !strings.Contains(err.Error(), "scopes") {
		t.Fatalf("a 403 should explain the token scopes, got %v", err)
	}
}

func TestAdminClientDisabled(t *testing.T) {
	a := newAdminClient(nil, "", "", false, true)
	if _, _, err := a.services(context.Background(), "3scale"); err == nil {
		t.Fatal("a disabled client must report that it is disabled")
	}
}

func TestProxyConfigRedactsOIDCCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"proxy":{"endpoint":"https://api.example.com:443",
			"api_backend":"https://upstream.internal:8080",
			"credentials_location":"query",
			"secret_token":"Shared_secret_sent_from_proxy_to_API_backend",
			"oidc_issuer_endpoint":"https://id:sso-secret@keycloak.example.com/auth/realms/r"}}`)
	}))
	defer srv.Close()
	a := newAdminClient(nil, srv.URL, "tok", false, false)
	proxy, err := a.proxyConfig(context.Background(), "3scale", "2")
	if err != nil {
		t.Fatalf("proxyConfig: %v", err)
	}
	if proxy["api_backend"] != "https://upstream.internal:8080" {
		t.Errorf("the private base URL should be reported: %v", proxy)
	}
	if _, ok := proxy["secret_token"]; ok {
		t.Error("secret_token must never be returned")
	}
	if strings.Contains(proxy["oidc_issuer_endpoint"], "sso-secret") {
		t.Errorf("the OIDC client secret leaked: %q", proxy["oidc_issuer_endpoint"])
	}
}
