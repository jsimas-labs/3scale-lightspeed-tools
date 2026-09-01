# APIcast metrics reference and how to interpret them

APIcast exposes Prometheus metrics on port **9421** at `/metrics`. On OpenShift
they are collected by user workload monitoring and queried through the Thanos
querier. `3scale_analyze_api_metrics`, `3scale_list_apis` and
`3scale_traffic_overview` are built on the metrics below.

## The metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `upstream_status` | counter | `status`, plus `service_id` and `service_system_name` with extended metrics | HTTP status APIcast returned for each request. The basis of all traffic analysis. |
| `total_response_time_seconds` | histogram | same | Time from request arrival to response sent: upstream time **plus** everything APIcast does (authrep against 3scale backend, policy chain, TLS). |
| `upstream_response_time_seconds` | histogram | same | Time spent waiting for the upstream API (the Private Base URL). |
| `threescale_backend_calls` | counter | `endpoint` (`authrep`, `auth`, `report`), `status` | Calls APIcast makes to 3scale backend to authorise and report usage. **No service label — gateway-wide.** |
| `nginx_http_connections` | gauge | `state` (`accepted`, `active`, `handled`, `reading`, `writing`, `waiting`, `total`) | Connection pressure on the gateway. |
| `nginx_error_log` | counter | `level` | Entries written to the nginx error log, by severity. |
| `nginx_metric_errors_total` | counter | – | Failures of the metrics library itself. |
| `openresty_shdict_capacity` / `openresty_shdict_free_space` | gauge | `dict` | Size and free space of the shared dictionaries, including the configuration cache. |
| `batching_policy_auths_cache_hits` / `_misses` | counter | – | Effectiveness of the 3scale Batcher policy cache. |

### The two labels that make per-API analysis possible

`service_id` and `service_system_name` are added to `upstream_status` and to
both histograms **only when `APICAST_EXTENDED_METRICS=true`**. Without them
every request is gateway-level and questions like "how many 5xx did the
Payments API return" cannot be answered. `3scale_check_metrics_pipeline`
reports which gateways are missing the setting.

`service_id` is the numeric id and is stable across renames;
`service_system_name` is the system name and can change. The display name shown
in the Admin Portal is in **neither** label — it comes from the 3scale Account
Management API, which the MCP server queries separately.

## Interpreting the numbers

### Status-code distribution

A healthy API is dominated by 2xx with a small, stable 4xx tail. What matters
is which error dominates, not the absolute count:

| Dominant code | Where the problem is | First checks |
|---|---|---|
| 502 | Between APIcast and your upstream | Private Base URL, upstream health, DNS resolution from the gateway pod, upstream TLS certificate trust |
| 503 | No healthy upstream, or 3scale backend unreachable | `threescale_backend_calls`, backend-listener pods, backend Redis |
| 504 | Upstream too slow for the APIcast timeout | upstream p99 latency, upstream capacity |
| 499 | Clients giving up before APIcast answers | almost always a slow upstream; look at latency, not at APIcast |
| 403 | Authentication rejected by APIcast | credential location in the product integration, application state, OIDC/zync sync |
| 404 | No mapping rule matched, or unknown host | mapping rules, promoted configuration version, public endpoint host |
| 429 | Plan limits or a rate-limit policy | application plan limits, rate-limit policy configuration |
| 500 | APIcast Lua error or upstream 500 | gateway logs, custom policies |

Useful thresholds when nothing better is known for the API: a 5xx share above
**5%** is a problem, above **20%** the API is effectively down. A 4xx share
above **30%** concentrated on 403 or 404 is a configuration problem rather than
misbehaving clients.

### Latency: separating APIcast from your API

The gap between the two histograms is the whole point:

```
APIcast overhead ≈ total_response_time_seconds − upstream_response_time_seconds
```

- **Overhead small and stable** (typically a few milliseconds to tens of
  milliseconds): the gateway is fine. If responses are slow, the upstream is
  slow.
- **Overhead large** (comparable to or larger than upstream time): the delay is
  inside the gateway. Causes, in order of likelihood: slow authrep calls to
  3scale backend (check `threescale_backend_calls` and backend Redis latency),
  an expensive policy in the chain, configuration reloads under
  `APICAST_CONFIGURATION_LOADER=lazy`, DNS resolution of the upstream host, or
  CPU saturation of the gateway pod.
- **Upstream p99 close to the APIcast timeout**: expect 504s and 499s to appear
  before the average moves. Percentiles surface this; averages hide it.

### 3scale backend calls

Every request APIcast authorises produces a call to 3scale backend
(`authrep`, or `auth` + `report` when the Batcher policy is not in use). If a
non-negligible share of those calls is not 2xx, authorisation is degraded for
**every** API on that gateway: check `backend-listener` pods and the backend
Redis connection with `3scale_check_database_config`. Rising
`batching_policy_auths_cache_misses` with the Batcher policy enabled means the
cache is not absorbing load, which pushes latency into the overhead above.

### Shared dictionaries

`openresty_shdict_free_space / openresty_shdict_capacity` below roughly 10% for
the configuration dictionary means APIcast may be unable to load or refresh
service configuration — a cause of sudden 404s on APIs that used to work. It is
reported by `3scale_traffic_overview`.

### Connections and error log

Rising `nginx_http_connections{state="waiting"}` with flat throughput indicates
saturation: consider more replicas or more `APICAST_WORKERS`. A jump in
`nginx_error_log{level="error"}` almost always coincides with 502/500 spikes
and tells you to read the pod logs.

## Related settings

- `APICAST_EXTENDED_METRICS=true` — required for per-API labels. Set it through
  the operator (`spec.extendedMetrics` on an `APIcast` CR;
  `spec.apicast.stagingSpec`/`productionSpec` on an APIManager, where the
  operator version supports it) rather than by editing the Deployment, which the
  operator reverts on the next reconcile. Confirm the result with
  `3scale_list_apicast_gateways`, which reads the variable from the running pod
  spec.
- `APICAST_RESPONSE_CODES=true` — makes APIcast report each response code to
  3scale backend, which populates the per-status-code analytics in the Admin
  Portal. Independent from Prometheus metrics; enable both.
- `APICAST_CONFIGURATION_CACHE` — how long production APIcast keeps a
  configuration before reloading; explains "I promoted but nothing changed".
- `APICAST_WORKERS` — nginx worker processes per pod.
