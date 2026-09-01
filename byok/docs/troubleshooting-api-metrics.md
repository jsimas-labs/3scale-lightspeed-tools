# Runbooks: diagnosing an API from its metrics

Each runbook starts from a metric signature that `3scale_analyze_api_metrics`
or `3scale_traffic_overview` can show, and ends with a fix. Confirm the
signature before acting on the diagnosis.

## Signature: 5xx spike, mostly 502, upstream latency unchanged

**Reading.** APIcast is reaching the upstream but getting nothing usable back,
or cannot open the connection at all. Latency does not move because failures
return fast.

**Diagnosis path.**
1. Confirm the Private Base URL in the integration section of the analysis
   output (`api_backend`).
2. Check the upstream from inside the gateway pod — DNS is resolved by the pod,
   not by the cluster router:
   `oc exec deployment/apicast-production -n <ns> -- curl -sv https://<upstream-host>/`
3. If the upstream is HTTPS with a private CA, APIcast must trust it. Either
   add the CA to the gateway or set `APICAST_OPENSSL_VERIFY=false` only for a
   test (never as a permanent fix in production).
4. Look for `upstream` errors in the gateway log:
   `3scale_get_pod_logs` on the pod named in the per-gateway table.

**Common causes.** Upstream down or scaled to zero; wrong Private Base URL
after a promote; NetworkPolicy or egress firewall blocking the gateway;
untrusted upstream certificate; upstream returning malformed responses.

## Signature: 5xx spike, mostly 503, `threescale_backend_calls` not 2xx

**Reading.** The failure is not in your API — APIcast cannot authorise
requests. This affects every API on the gateway, which the per-gateway and
overview tables will confirm.

**Diagnosis path.**
1. `3scale_traffic_overview` — is the 5xx rate high for all APIs on that
   gateway?
2. `3scale_list_pods` in the API Manager namespace: are `backend-listener`
   pods Ready?
3. `3scale_check_database_config` with connectivity testing: backend Redis is
   external since 3scale 2.16, so a Redis outage or an expired TLS certificate
   on the Redis endpoint breaks authorisation without any pod looking unhealthy.
4. `3scale_get_pod_logs` on `backend-listener`.

## Signature: 504 and 499 rising together, upstream p99 very high

**Reading.** The upstream is too slow. 504 is APIcast giving up; 499 is the
client giving up first. The gateway is a victim, not the cause — its overhead
(total minus upstream) stays small.

**Diagnosis path.** Compare upstream p95 and p99: a p99 far above p95 means a
subset of requests (one endpoint, one dependency, cold caches) is slow rather
than the whole API. Fix the upstream, or raise the APIcast timeout only if the
slow behaviour is expected.

## Signature: sudden 404 on an API that used to work

**Reading.** Either no mapping rule matched, or the request host is not a
configured public endpoint, or the gateway is serving a stale configuration.

**Diagnosis path.**
1. Compare the timeline with the last promote. Production APIcast reloads
   configuration only every `APICAST_CONFIGURATION_CACHE` seconds (default 300)
   — until then it keeps serving the previous configuration.
2. Check that the request host matches `endpoint` (production) or
   `sandbox_endpoint` (staging) in the integration section.
3. Mapping rules are prefix matches by default; `$` forces an exact match.
4. If the configuration dictionary is nearly full
   (`openresty_shdict_free_space`), APIcast may have failed to load the
   configuration: restart the deployment and give it more memory.
5. Force a reload: `oc rollout restart deployment/apicast-production -n <ns>`.

## Signature: 403 dominates, 2xx collapses at a specific moment

**Reading.** Credentials stopped being accepted.

**Diagnosis path.**
1. Check the credential location (`credentials_location`) — query string,
   headers or basic auth — against what clients send.
2. For API key / app_id modes: was the application suspended, or the key
   rotated?
3. For OIDC: is the token issuer the configured one, and did zync synchronise
   the client into the identity provider? See the zync runbook. A zync backlog
   produces exactly this pattern for newly created applications.
4. Check `threescale_backend_calls`: if authorisation calls themselves fail,
   the cause is the backend, not the credentials.

## Signature: 429 rising

**Reading.** Applications are hitting plan limits, or an APIcast rate-limit
policy is throttling them. Not a fault — but confirm it is the intended limit
and not a misconfigured policy applied to the wrong scope.

## Signature: latency high, but upstream latency low

**Reading.** The gateway itself is adding the time. The analysis output states
the p95 overhead explicitly.

**Diagnosis path.**
1. `threescale_backend_calls` volume and the state of backend Redis: slow
   authrep is the most common cause. The Batcher policy reduces this
   dramatically for high-traffic APIs.
2. Review the policy chain for expensive custom policies.
3. Check gateway CPU throttling and `nginx_http_connections{state="waiting"}`
   for saturation; scale replicas or raise `APICAST_WORKERS`.
4. With `APICAST_CONFIGURATION_LOADER=lazy`, the first request for a host pays
   for a configuration fetch from system — the signature is periodic latency
   spikes rather than a constant offset.

## Signature: traffic drops to zero

**Reading.** Requests are no longer reaching the gateway, or the gateway stopped
serving. This is a routing question, not an API question.

**Diagnosis path.**
1. `3scale_list_apicast_gateways` — are the pods Ready?
2. `3scale_check_routes` — is the route still admitted? Missing routes point at
   zync-que.
3. `3scale_get_events` in the gateway namespace for evictions, OOM kills or
   probe failures.
4. If only one API went quiet while others continue, look at DNS for that
   API's public host and at recent configuration changes.

## Signature: no data at all for any API

Run `3scale_check_metrics_pipeline`. In order of frequency the cause is:
user workload monitoring disabled; no ServiceMonitor in the gateway namespace;
`APICAST_EXTENDED_METRICS` not enabled (metrics exist but carry no per-API
labels, so APIs appear as `<unlabelled>`); the gateway namespace not covered by
the query scope; or genuinely no traffic in the window.
