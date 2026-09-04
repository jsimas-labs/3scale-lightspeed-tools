# 3scale Lightspeed Tools

**OpenShift Lightspeed (OLS)** tools for diagnosing
**Red Hat 3scale API Management**, consisting of two images publishable to
quay.io:

1. **Go MCP server** ([mcp-server/](mcp-server/)) — exposes *read-only*
   3scale troubleshooting tools via the Model Context Protocol
   (streamable HTTP transport), consumed by OLS through
   `spec.mcpServers`.
2. **BYOK image** ([byok/](byok/)) — RAG knowledge base (FAISS) with
   3scale troubleshooting runbooks, consumed by OLS through
   `spec.ols.rag`.

```
┌──────────────────────────────── OpenShift ─────────────────────────────────┐
│  namespace openshift-lightspeed                                            │
│  ┌──────────────────────┐                                                  │
│  │ OLS (lightspeed-app) │                     namespace 3scale             │
│  │  ├─ RAG: BYOK image  │      ┌──── K8s API ──▶ APIManager, system,       │
│  │  │   (how to read    │      │                 backend, zync, routes     │
│  │  │    the data)      │      │                                           │
│  │  └─ MCP: 3scale-     │──────┤              namespaces team-a, team-b…   │
│  │     troubleshoot     │      ├──── K8s API ──▶ APIcast gateways deployed  │
│  └──────────────────────┘      │                 by the APIcast operator    │
│                                │                                           │
│                                │              namespace openshift-monitoring│
│                                └──── PromQL ──▶ Thanos querier             │
│                                                 (APIcast metrics from       │
│                                                  every namespace)           │
└────────────────────────────────────────────────────────────────────────────┘
```

APIcast gateways are discovered **cluster-wide**: the APIcast operator can
deploy self-managed gateways (kind `APIcast`) in any namespace, so the server
never assumes they sit next to the APIManager.

## Available MCP tools

All tools are prefixed with `3scale_` and are strictly read-only.

### API traffic analysis (from APIcast metrics)

| Tool | Description |
|---|---|
| `3scale_analyze_api_metrics` | **Main tool.** Analyses one API found by product name, system name or service id: request volume, full HTTP status-code breakdown with what each code means in APIcast, 2xx/4xx/5xx split, traffic and errors per gateway, latency (avg/p95/p99) for the client-observed total *and* for the upstream API — isolating APIcast overhead —, calls to 3scale backend, integration settings (public endpoint, Private Base URL), a request/error timeline, the state of the serving pods, and automatic findings naming the probable cause |
| `3scale_list_apis` | APIs receiving traffic in a window, ranked, with 4xx/5xx counts, error rate and serving namespaces; also lists products configured but idle |
| `3scale_traffic_overview` | Gateway-fleet view: totals and status classes, per-gateway error rates, APIs producing the most 5xx, 3scale backend calls, nginx error log and connections, shared dictionaries near capacity |
| `3scale_list_apicast_gateways` | Every APIcast gateway in the cluster, in any namespace: managing operator, readiness, image, and the `APICAST_*` settings that matter (extended metrics, response codes, configuration cache, portal endpoint with credentials redacted), plus ServiceMonitor presence |
| `3scale_check_metrics_pipeline` | Why metrics are missing: gateway settings, ServiceMonitors, user workload monitoring, Prometheus reachability, available metrics and per-API labels, scrape target health — returns the exact YAML to apply |
| `3scale_query_metrics` | Arbitrary read-only PromQL (instant or range), for anything the tools above do not cover |

### Installation troubleshooting

| Tool | Description |
|---|---|
| `3scale_diagnose` | Health summary in one call: APIManager, deployments, pods, PVCs, databases and Warning events |
| `3scale_get_apimanager_status` | Status conditions of the APIManager CR (apps.3scale.net) |
| `3scale_list_pods` | Pods with phase, readiness, restarts, and failure reasons |
| `3scale_get_pod_logs` | Logs from a pod (container, tail, previous/crashed instance) |
| `3scale_get_deployments` | Desired/ready replicas and unhealthy conditions |
| `3scale_get_events` | Namespace events (optionally Warning only), most recent first |
| `3scale_check_routes` | Routes (portals and gateways) with host, TLS, and admission status |
| `3scale_check_database_config` | Database configuration from secrets (`backend-redis`, `system-redis`, `system-database`, `zync`) with redacted credentials, `externalComponents` flags, and optional TCP connectivity test — essential on 2.16, where Redis and RDBMS are external |
| `3scale_check_pvcs` | PVCs with phase, capacity, and storage class |

The installation tools accept an optional `namespace` defaulting to
`THREESCALE_NAMESPACE` (default `3scale`).

The metric tools accept the same `namespace` (plus an optional `gateway`) and a
`window` such as `15m`, `1h`, `24h` or `7d` (default `1h`).

**APIs and gateways are looked up cluster-wide and are never hidden by a
namespace argument.** The reference install has the APIManager in one namespace
and self-managed APIcast gateways — created by the APIcast operator — in others;
the APIManager namespace then holds no gateway and no traffic metrics at all.
Narrowing the search by namespace would report a perfectly healthy API as
missing. Once the API is located, metric queries narrow to the namespaces
actually serving it and the report names them. A `namespace` argument restricts
deliberately, but when it matches nothing the tools widen the search and say so
rather than returning zeros.

`THREESCALE_NAMESPACE` is therefore **not** a metric filter: it identifies the
API Manager, and is used to reach its Admin Portal for product display names and
by the installation tools.

### Requirements for per-API metrics

1. **User workload monitoring enabled** on the cluster — otherwise APIcast is
   never scraped.
2. **A ServiceMonitor (or PodMonitor) in each namespace hosting gateways**,
   scraping port 9421.
3. **`APICAST_EXTENDED_METRICS=true` on each gateway** — this is what adds the
   `service_id` and `service_system_name` labels that make per-API analysis
   possible. Without it, traffic shows up as `<unlabelled>`.

Run `3scale_check_metrics_pipeline` to verify all three at once; reference
manifests are in [deploy/40-apicast-monitoring-example.yaml](deploy/40-apicast-monitoring-example.yaml).

Resolving an API by the **display name** shown in the Admin Portal additionally
uses the 3scale Account Management API (the `system-provider` route plus
`ADMIN_ACCESS_TOKEN` from the `system-seed` secret). This is optional: with it
disabled or unreachable, APIs are still addressable by system name or numeric
service id.

### Configuration

| Flag | Environment variable | Default |
|---|---|---|
| `-namespace` | `THREESCALE_NAMESPACE` | `3scale` |
| `-prometheus-url` | `PROMETHEUS_URL` | `https://thanos-querier.openshift-monitoring.svc.cluster.local:9091` (empty disables the metric tools) |
| `-prometheus-token` | `PROMETHEUS_TOKEN` | the pod ServiceAccount token |
| `-prometheus-insecure` | `PROMETHEUS_INSECURE` | `false` |
| `-admin-url` | `THREESCALE_ADMIN_URL` | discovered from the `system-provider` route |
| `-admin-token` | `THREESCALE_ACCESS_TOKEN` | `ADMIN_ACCESS_TOKEN` from the `system-seed` secret |
| `-admin-insecure` | `THREESCALE_ADMIN_INSECURE` | `false` |
| `-disable-admin-api` | `THREESCALE_DISABLE_ADMIN_API` | `false` |

## Prerequisites

- OpenShift 4.x with the **OpenShift Lightspeed operator** installed and a
  working OLSConfig `cluster` (LLM provider configured)
- 3scale API Management installed via the operator (`APIManager` CR)
- `podman`, `oc`, `make`, Go ≥ 1.25 (local build only)
- For per-API metrics: OpenShift **user workload monitoring** enabled and the
  APIcast gateways scraped (see [deploy/40-apicast-monitoring-example.yaml](deploy/40-apicast-monitoring-example.yaml))
- A [quay.io](https://quay.io) account with a **public** repository (or a pull
  secret configured on the cluster for private repositories)

---

## 1. Build and publish the MCP server (quay.io)

```bash
podman login quay.io

# build + push (replace CHANGE_ME with your user/organization)
make image-build image-push REGISTRY_ORG=<your-user>
# equivalent to:
#   podman build -t quay.io/<your-user>/3scale-troubleshoot-mcp:latest \
#     -f mcp-server/Containerfile mcp-server/
#   podman push quay.io/<your-user>/3scale-troubleshoot-mcp:latest
```

Local binary build (development): `make build`. To run locally against the
current kubeconfig:

```bash
cd mcp-server && go run . -transport stdio -namespace 3scale
# or HTTP: go run . -transport http -listen :8080  →  endpoint http://localhost:8080/mcp
```

## 2. Build and publish the BYOK image (quay.io)

The image is generated by the official `lightspeed-rag-tool` from the
markdown files in [byok/docs/](byok/docs/). Details in [byok/README.md](byok/README.md).

```bash
podman login registry.redhat.io   # required to pull the rag-tool
podman login quay.io

make byok-push REGISTRY_ORG=<your-user>
# generates the FAISS index, loads byok-image.tar, tags and publishes
# quay.io/<your-user>/3scale-docs-byok:latest
```

## 3. Cluster installation

### 3.1 Deploy the MCP server

```bash
# RBAC (read pods/logs/events/PVCs/deployments/routes/apimanagers/apicasts,
# monitors, plus cluster-monitoring-view for metrics)
# + Deployment/Service in the openshift-lightspeed namespace
make deploy MCP_IMAGE=quay.io/<your-user>/3scale-troubleshoot-mcp:latest

# if 3scale is not in the "3scale" namespace, adjust:
oc set env deployment/threescale-troubleshoot-mcp -n openshift-lightspeed \
  THREESCALE_NAMESPACE=<3scale-namespace>

# verification
oc get pods -n openshift-lightspeed -l app=threescale-troubleshoot-mcp

# check that APIcast metrics are actually available
make check-metrics
oc exec -n openshift-lightspeed deploy/threescale-troubleshoot-mcp -- \
  /bin/sh -c 'true' 2>/dev/null || true
curl -s http://$(oc get svc threescale-troubleshoot-mcp -n openshift-lightspeed \
  -o jsonpath='{.spec.clusterIP}'):8080/healthz   # from a cluster pod
```

### 3.2 OLSConfig configuration

Edit the OLSConfig `cluster` (`oc edit olsconfig cluster`) and **merge** the
fields below — do not replace the existing provider configuration. Full
example in [deploy/30-olsconfig-example.yaml](deploy/30-olsconfig-example.yaml):

```yaml
spec:
  featureGates:
    - MCPServer            # enables MCP integration (Tech Preview)
  mcpServers:
    - name: 3scale-troubleshoot
      url: "http://threescale-troubleshoot-mcp.openshift-lightspeed.svc.cluster.local:8080/mcp"
      timeout: 120           # metric analyses run several Prometheus queries
  ols:
    rag:
      - image: quay.io/<your-user>/3scale-docs-byok:latest
        indexID: vector_db_index      # optional (default)
        indexPath: /rag/vector_db     # optional (default)
```

The OLS operator restarts `lightspeed-app-server` applying the RAG and MCP.

### 3.3 End-to-end verification

```bash
oc get pods -n openshift-lightspeed
oc logs deployment/lightspeed-app-server -n openshift-lightspeed | grep -i -e mcp -e rag
```

In the OpenShift console, open Lightspeed and ask, for example:

- *"Diagnose my 3scale installation"* → should trigger `3scale_diagnose`
- *"How is the Echo API doing in the last hour?"* → `3scale_analyze_api_metrics`
- *"Why is my API returning 502?"* → `3scale_analyze_api_metrics`, with the
  answer explaining the upstream/gateway split from the RAG knowledge base
- *"Which of my APIs has the most errors today?"* → `3scale_list_apis` with a
  `24h` window
- *"Where are my APIcast gateways?"* → `3scale_list_apicast_gateways`
- *"Why are my 3scale admin portal routes missing?"* → should combine the zync
  runbook (RAG) with `3scale_check_routes`/`3scale_get_pod_logs` (MCP)

## Repository structure

```
├── mcp-server/          # Go MCP server
│   ├── main.go          #   flags, transports, server instructions
│   ├── k8s.go           #   Kubernetes clients and GVRs
│   ├── tools.go         #   tool registration + installation troubleshooting
│   ├── discovery.go     #   cluster-wide APIcast gateway discovery
│   ├── prometheus.go    #   dependency-free Prometheus/Thanos client
│   ├── apimetrics.go    #   API resolution, status/latency/trend primitives
│   ├── analysis.go      #   metric report builders and interpretation rules
│   ├── adminapi.go      #   optional 3scale Admin API product catalog
│   └── dbconfig.go      #   external database configuration
├── byok/                # markdown docs + BYOK image build.sh
│   └── docs/            # 3scale runbooks, metric reference, MCP playbook
├── deploy/              # RBAC, Deployment/Service, OLSConfig and monitoring examples
├── Makefile
└── README.md
```

## Security

- The MCP server is **read-only**: no tool creates, updates, or deletes
  resources (RBAC limited to `get`/`list`).
- Secrets: RBAC allows `get` only on named secrets — the database connection
  secrets (`backend-redis`, `system-redis`, `system-database`, `zync`,
  `system-memcache`), required because since 3scale 2.16 databases are
  external, and `system-seed`, used to read the Admin API token that resolves
  API display names. Credentials are **never** returned: URLs are redacted,
  password keys appear only as `[set, redacted]`, and
  `THREESCALE_PORTAL_ENDPOINT` is shown as `https://[credentials redacted]@host`
  because the 3scale access token is the URL user-info. Set
  `THREESCALE_DISABLE_ADMIN_API=true` to drop the `system-seed` dependency
  entirely.
- Metrics access uses the `cluster-monitoring-view` ClusterRole, which is
  read-only over the cluster monitoring stack.
- RBAC is cluster-scoped because APIcast gateways can be in any namespace. To
  restrict it, replace the ClusterRoleBinding with RoleBindings in the
  namespaces that actually hold 3scale and its gateways; gateways outside those
  namespaces then simply will not be discovered.
- Pod logs may contain sensitive data; OLS access already requires cluster
  authorization, but restrict the ClusterRole to specific namespaces
  (switch to Role + RoleBinding) if needed.
- The container runs as non-root, with a read-only rootfs and no capabilities.

## References

- [Bring your own knowledge to OpenShift Lightspeed (Red Hat Blog)](https://www.redhat.com/en/blog/bring-your-own-knowledge-openshift-lightspeed)
- [OpenShift Lightspeed — Configuration (official docs)](https://docs.redhat.com/en/documentation/red_hat_openshift_lightspeed/1.0/html/configure/ols-configuring-openshift-lightspeed)
- [OLSConfig API reference](https://docs.redhat.com/en/documentation/red_hat_openshift_lightspeed/1.0/html/configure/olsconfig-api)
- [MCP Go SDK (official)](https://github.com/modelcontextprotocol/go-sdk)
- [3scale operator](https://github.com/3scale/3scale-operator)
