# Playbook: troubleshooting 3scale with the MCP tools

This playbook maps symptoms to the read-only MCP tools exposed by the
`3scale-troubleshoot` server. Every tool name starts with `3scale_`. Use the
tools to gather evidence before stating a cause: each answer should name the
namespace, the gateway deployment and the concrete status codes or counts that
justify it.

## The tools

| Tool | Answers |
|---|---|
| `3scale_diagnose` | Overall health of an API Manager install (APIManager conditions, deployments, pods, PVCs, databases, warning events) |
| `3scale_list_apicast_gateways` | Where every APIcast gateway is, in any namespace, and how it is configured |
| `3scale_list_apis` | Which APIs are receiving traffic, ranked, with 4xx/5xx and error rate |
| `3scale_analyze_api_metrics` | Everything about ONE API: status-code breakdown, latency, per-gateway split, timeline, findings |
| `3scale_traffic_overview` | Gateway-fleet view: totals, per-gateway error rates, worst APIs, backend calls, nginx health |
| `3scale_check_metrics_pipeline` | Why metrics are missing or lack per-API labels |
| `3scale_query_metrics` | Raw PromQL, for questions the tools above do not cover |
| `3scale_list_pods`, `3scale_get_deployments`, `3scale_get_events`, `3scale_get_pod_logs` | Workload state and logs |
| `3scale_check_routes` | Routes, hosts, TLS termination, admission status |
| `3scale_check_database_config` | External Redis/RDBMS connection settings (redacted) and TCP reachability |
| `3scale_check_pvcs` | PersistentVolumeClaim state |

## Standard workflows

### "API X is returning errors" / "API X is slow"

1. `3scale_analyze_api_metrics` with the name the user typed. The tool accepts
   the Admin Portal product name, the system name, or the numeric service id.
2. Read the status-code table first. One dominant error code identifies the
   class of problem immediately (see `apicast-metrics-reference.md`).
3. Read the latency table. If `total` is much larger than `upstream`, the delay
   is inside APIcast; if they track each other, the upstream API is slow.
4. Read the per-gateway table. Errors on one gateway only means a gateway
   problem (bad config reload, crashed pod, stale cache), not an API problem.
5. Read the timeline. A step change points at a deploy or a promote; a gradual
   ramp points at saturation or a leak.
6. Follow the findings to the specific runbook, then confirm with
   `3scale_get_pod_logs` on the gateway pod the tool named.

### "Which of my APIs is unhealthy?"

`3scale_list_apis` over a meaningful window (`24h` for a daily review, `1h` for
an incident), then `3scale_analyze_api_metrics` on the worst offender. APIs that
appear under "configured but with no traffic" are as interesting as the failing
ones: an API that suddenly receives zero requests usually has a routing, DNS or
promote problem rather than an application problem.

### "The gateway is unhealthy" / "everything is failing"

`3scale_traffic_overview` first. If 5xx is spread across every API and gateway,
suspect a shared dependency: 3scale backend (`threescale_backend_calls` not
2xx), backend Redis, or the upstream network. If it is concentrated on one
gateway, suspect that deployment.

### "I get no metrics"

`3scale_check_metrics_pipeline`. It checks, in order: gateways found and their
`APICAST_EXTENDED_METRICS` setting, ServiceMonitors/PodMonitors, user workload
monitoring, Prometheus reachability, which APIcast metrics exist, whether the
per-API labels have values, and scrape target health. It returns the exact YAML
to apply. Do not guess before running it.

### "Where is my gateway?"

`3scale_list_apicast_gateways`. Do not assume the gateways live next to the
APIManager: see `apicast-operator-multi-namespace.md`.

## Reading the output

- **Windows.** Metric tools take `window` (`15m`, `1h`, `24h`, `7d`, default
  `1h`). Counts are `increase()` over that window, so they are totals for the
  period, not instantaneous rates. Widen the window when traffic is low.
- **`<unlabelled>` in `3scale_list_apis`** means traffic from a gateway running
  without `APICAST_EXTENDED_METRICS=true`. That traffic exists but cannot be
  attributed to an API.
- **Backend call tables are gateway-wide**, not per API: `threescale_backend_calls`
  has no service label. Failures there degrade every API on that gateway.
- **Latency is measured by the gateway**, so it excludes client network time
  but includes everything APIcast does.
- **Credentials are always redacted.** `THREESCALE_PORTAL_ENDPOINT` is shown as
  `https://[credentials redacted]@host` because the access token is the URL
  user-info; never ask the user to paste the unredacted value.

## What the tools cannot tell you

- Per-API 3scale backend latency (`threescale_backend_calls` is gateway-wide).
- Which application or account made a request: APIcast metrics have no
  application labels. Use the Admin Portal analytics for that, which requires
  `APICAST_RESPONSE_CODES=true`.
- Anything about a tenant other than the default one when resolving product
  names: the Admin API lookup uses the `system-seed` token of the default
  tenant. For other tenants, ask the user for the system name or service id.
