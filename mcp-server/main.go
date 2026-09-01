// 3scale-troubleshoot-mcp is an MCP (Model Context Protocol) server that
// exposes read-only troubleshooting tools for Red Hat 3scale API Management
// running on OpenShift. It is designed to be consumed by OpenShift Lightspeed
// via the OLSConfig `mcpServers` integration (streamable HTTP transport), but
// it also supports stdio for local use with any MCP client.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "3scale-troubleshoot"
	serverVersion = "2.0.0"
)

const serverInstructions = `Read-only troubleshooting tools for Red Hat 3scale API Management on OpenShift, including per-API traffic analysis from APIcast metrics.

All tools are prefixed with 3scale_.

Where things live:
- The API Manager (system, backend, zync, apicast-staging, apicast-production) lives in one namespace, by default the one this server was configured with.
- APIcast gateways are NOT limited to that namespace: the APIcast operator can deploy self-managed gateways (kind APIcast, apps.3scale.net/v1alpha1) in any namespace. Metric tools therefore search the whole cluster unless a namespace is given.

Choosing a tool:
1. "Why is API X failing / slow / returning errors?" -> 3scale_analyze_api_metrics with the product name, system name or service id. It resolves the API, breaks traffic down by HTTP status code, compares upstream and total latency, and reports probable causes.
2. "Which APIs exist / which one is unhealthy?" -> 3scale_list_apis, then drill into the worst.
3. "How is the gateway fleet doing?" -> 3scale_traffic_overview.
4. "Where are the gateways and how are they configured?" -> 3scale_list_apicast_gateways.
5. A metrics tool returned no data -> 3scale_check_metrics_pipeline; the usual causes are user workload monitoring being disabled, a missing ServiceMonitor, or APICAST_EXTENDED_METRICS not being true (without it, metrics carry no per-API labels).
6. Installation health -> 3scale_diagnose, then 3scale_list_pods, 3scale_get_deployments, 3scale_get_events, 3scale_get_pod_logs, 3scale_check_routes, 3scale_check_pvcs.
7. Database problems -> 3scale_check_database_config: since 3scale 2.16 the Redis databases (and usually the system RDBMS) are EXTERNAL, configured through the backend-redis, system-redis, system-database and zync secrets — do not assume database pods exist in the namespace.
8. Anything the dedicated tools do not cover -> 3scale_query_metrics with raw PromQL.

Interpreting metrics: upstream_status counts the status APIcast returned to the client; total_response_time_seconds minus upstream_response_time_seconds is the overhead APIcast itself adds (authrep to 3scale backend, policies, TLS); threescale_backend_calls shows whether authorisation against 3scale backend is healthy, and is gateway-wide rather than per API.

Never assume a failure without evidence from a tool call, and prefer naming the exact namespace, deployment and status code in the answer.`

func main() {
	var (
		transport  = flag.String("transport", envOr("MCP_TRANSPORT", "http"), "MCP transport: 'http' (streamable HTTP) or 'stdio'")
		listenAddr = flag.String("listen", envOr("MCP_LISTEN", ":8080"), "listen address for the HTTP transport")
		namespace  = flag.String("namespace", envOr("THREESCALE_NAMESPACE", "3scale"), "default namespace of the 3scale API Manager (APIcast gateways are discovered cluster-wide)")

		promURL      = flag.String("prometheus-url", envOr("PROMETHEUS_URL", defaultPrometheusURL), "Prometheus/Thanos endpoint used for APIcast metrics; empty disables the metric tools")
		promToken    = flag.String("prometheus-token", os.Getenv("PROMETHEUS_TOKEN"), "bearer token for Prometheus (defaults to the pod ServiceAccount token)")
		promInsecure = flag.Bool("prometheus-insecure", envOr("PROMETHEUS_INSECURE", "false") == "true", "skip TLS verification when querying Prometheus")

		adminURL      = flag.String("admin-url", os.Getenv("THREESCALE_ADMIN_URL"), "3scale admin portal base URL (defaults to the system-provider route in the 3scale namespace)")
		adminToken    = flag.String("admin-token", os.Getenv("THREESCALE_ACCESS_TOKEN"), "3scale Admin API access token (defaults to ADMIN_ACCESS_TOKEN in the system-seed secret)")
		adminInsecure = flag.Bool("admin-insecure", envOr("THREESCALE_ADMIN_INSECURE", "false") == "true", "skip TLS verification when calling the 3scale Admin API")
		adminDisabled = flag.Bool("disable-admin-api", envOr("THREESCALE_DISABLE_ADMIN_API", "false") == "true", "do not call the 3scale Admin API; APIs can then only be matched by system name or service id")
	)
	flag.Parse()

	kc, err := newK8sClients()
	if err != nil {
		log.Fatalf("failed to initialize Kubernetes clients: %v", err)
	}
	kc.defaultNamespace = *namespace

	d := &deps{
		kc:    kc,
		prom:  newPromClient(*promURL, *promToken, *promInsecure),
		admin: newAdminClient(kc, *adminURL, *adminToken, *adminInsecure, *adminDisabled),
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Title:   "3scale API Management Troubleshooting",
		Version: serverVersion,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	registerTools(server, d)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *transport {
	case "stdio":
		log.Printf("%s %s: serving MCP on stdio (API Manager namespace %q, metrics %s)", serverName, serverVersion, *namespace, metricsStatus(d))
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
			log.Fatalf("stdio server error: %v", err)
		}
	case "http":
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
		mux := http.NewServeMux()
		mux.Handle("/mcp", handler)
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "ok")
		})
		srv := &http.Server{
			Addr:              *listenAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		log.Printf("%s %s: serving MCP on %s/mcp (API Manager namespace %q, metrics %s)", serverName, serverVersion, *listenAddr, *namespace, metricsStatus(d))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	default:
		log.Fatalf("unknown transport %q (use 'http' or 'stdio')", *transport)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// metricsStatus describes the metrics backend for the startup log line.
func metricsStatus(d *deps) string {
	if !d.prom.enabled() {
		return "disabled"
	}
	return "via " + d.prom.baseURL
}
