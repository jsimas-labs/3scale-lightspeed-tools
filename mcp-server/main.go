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
	serverVersion = "0.1.0"
)

const serverInstructions = `Read-only troubleshooting tools for Red Hat 3scale API Management on OpenShift.

Typical workflow:
1. Call diagnose_3scale first for an overall health summary of the 3scale installation.
2. Drill down with list_3scale_pods, get_deployments, get_events, check_routes or check_pvcs.
3. Use get_pod_logs to inspect logs of a failing component (apicast, system-app, system-sidekiq, backend, zync).
4. For database problems use check_database_config: since 3scale 2.16 the Redis databases (and usually the system RDBMS) are EXTERNAL, configured through the backend-redis, system-redis, system-database and zync secrets — do not assume database pods exist in the namespace.
All tools default to the namespace where 3scale (APIManager) is installed; override with the "namespace" argument when needed.`

func main() {
	var (
		transport  = flag.String("transport", envOr("MCP_TRANSPORT", "http"), "MCP transport: 'http' (streamable HTTP) or 'stdio'")
		listenAddr = flag.String("listen", envOr("MCP_LISTEN", ":8080"), "listen address for the HTTP transport")
		namespace  = flag.String("namespace", envOr("THREESCALE_NAMESPACE", "3scale"), "default namespace of the 3scale installation")
	)
	flag.Parse()

	kc, err := newK8sClients()
	if err != nil {
		log.Fatalf("failed to initialize Kubernetes clients: %v", err)
	}
	kc.defaultNamespace = *namespace

	server := mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Title:   "3scale API Management Troubleshooting",
		Version: serverVersion,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	registerTools(server, kc)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *transport {
	case "stdio":
		log.Printf("%s %s: serving MCP on stdio (default namespace %q)", serverName, serverVersion, *namespace)
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
		log.Printf("%s %s: serving MCP on %s/mcp (default namespace %q)", serverName, serverVersion, *listenAddr, *namespace)
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
