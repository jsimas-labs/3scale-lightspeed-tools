# Troubleshooting APIcast (3scale API Gateway)

> Start from the data when you can: `3scale_analyze_api_metrics` gives the
> status-code distribution, latency split and per-gateway breakdown for a single
> API, which usually identifies which symptom below applies. See
> `apicast-metrics-reference.md` for what each metric means,
> `troubleshooting-api-metrics.md` for metric-driven runbooks, and
> `apicast-operator-multi-namespace.md` because gateways are not necessarily in
> the API Manager namespace.

## Symptom: HTTP 404 "no mapping rule matched" on every request

*Metric signature: `upstream_status{status="404"}` dominant for the service,
latency normal.*

- The request path does not match any **Mapping Rule** of the product.
  Check Product → Integration → Mapping Rules in the Admin Portal.
- Remember that mapping rules are prefix matches by default; use `$` to force
  exact match (`/v1/items$`).
- After changing rules you must **Promote** the configuration to staging and
  then to production. APIcast production only reloads configuration at the
  cache interval (`APICAST_CONFIGURATION_CACHE`, default 300s) or on pod restart.

## Symptom: HTTP 403 "Authentication failed" or "Authentication parameters missing"

*Metric signature: 403 dominant; check `threescale_backend_calls` to tell a
credential problem from a backend outage.*

- Wrong or missing credentials: user key (`user_key`), app_id/app_key pair or
  OIDC token, depending on the authentication mode of the product.
- Verify the credential location (query, headers, basic auth) configured in
  the product integration settings.
- If using OIDC, check that the token issuer matches the configured issuer URL
  and that zync has synchronized the client into the identity provider.
- Check `backend-listener` availability: if backend is down APIcast may deny
  requests. `oc logs deployment/backend-listener -n 3scale`.

## Symptom: HTTP 502/503 from APIcast

*Metric signature: 502 with unchanged upstream latency points at the upstream
connection; 503 together with non-2xx `threescale_backend_calls` points at
3scale backend or its Redis.*

- `502 Bad Gateway`: APIcast reached the upstream (Private Base URL) but the
  upstream failed or returned an invalid response. Check the upstream API and
  the **Private Base URL** in the product integration.
- `503`: usually no healthy upstream or APIcast worker saturation.
- TLS to upstream: if the upstream uses a certificate not trusted by APIcast,
  requests fail; either add the CA (`SSL_CERT_FILE` / custom policy) or fix the
  upstream certificate.
- DNS: APIcast resolves the upstream host inside the cluster. Test from the pod:
  `oc exec deployment/apicast-production -n 3scale -- curl -sv https://upstream-host`.

## Symptom: APIcast pods CrashLoopBackOff

- Check current and previous logs:
  `oc logs deployment/apicast-production -n 3scale --previous`.
- Common causes:
  - Cannot download configuration from system: `THREESCALE_PORTAL_ENDPOINT`
    unreachable or system-app not Ready — fix system first.
  - Invalid custom policy or configuration JSON — look for Lua errors in the log.
  - Liveness/readiness probe failures caused by slow config load on large
    installations — check events for `Unhealthy` probe messages.

## Symptom: changes promoted but production still behaves as before

- APIcast production caches configuration. Wait for the cache interval
  (default 300s) or restart the deployment:
  `oc rollout restart deployment/apicast-production -n 3scale`.
- Confirm the configuration version deployed: Admin Portal → Product →
  Integration → Configuration shows the promoted version history.

## Symptom: requests are slow

*Metric signature: compare `total_response_time_seconds` with
`upstream_response_time_seconds`. A large gap is APIcast overhead (authrep
latency, policies, configuration reloads); a small gap means the upstream API is
the slow part. 504 and 499 rise together when the upstream exceeds the timeout.*

See `troubleshooting-api-metrics.md` for the full latency runbook.

## Useful checks

```bash
# Gateway logs (staging picks changes almost immediately)
oc logs deployment/apicast-staging -n 3scale --tail=100
oc logs deployment/apicast-production -n 3scale --tail=100

# Call the gateway with debug tracing (staging only, use the SERVICE token)
curl -H "X-3scale-debug: <SERVICE_TOKEN>" https://api-<tenant>-apicast-staging.<domain>/path

# Verify APIcast can reach backend-listener
oc exec deployment/apicast-production -n 3scale -- \
  curl -s http://backend-listener:3000/status
```
