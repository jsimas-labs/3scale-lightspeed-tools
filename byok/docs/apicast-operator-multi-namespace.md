# APIcast gateways across namespaces (APIcast operator)

A very common wrong assumption when troubleshooting 3scale is that all APIcast
gateways live in the API Manager namespace. They do not.

## Two ways a gateway gets deployed

**1. Embedded gateways, deployed by the 3scale operator with the APIManager.**
Two Deployments in the API Manager namespace:

- `apicast-staging` — reloads configuration on almost every request, used to
  test before promoting.
- `apicast-production` — caches configuration for `APICAST_CONFIGURATION_CACHE`
  seconds (default 300).

They are labelled `threescale_component: apicast` and
`threescale_component_element: staging|production`. Configure them through
`spec.apicast.stagingSpec` / `spec.apicast.productionSpec` of the APIManager CR.

**2. Self-managed gateways, deployed by the APIcast operator.**
A custom resource of kind `APIcast` (`apps.3scale.net/v1alpha1`) creates a
Deployment named `apicast-<CR name>` **in the namespace of the CR**, which can
be any namespace in the cluster. Typical reasons: one gateway per team, per
environment, per network zone, or a gateway in a cluster different from the one
running the API Manager.

A self-managed gateway needs only credentials for the admin portal:

```yaml
apiVersion: apps.3scale.net/v1alpha1
kind: APIcast
metadata:
  name: team-a
  namespace: team-a
spec:
  adminPortalCredentialsRef:
    name: apicast-admin-portal-credentials   # secret with AdminPortalURL
  extendedMetrics: true
  replicas: 2
```

The secret holds `AdminPortalURL` in the form
`https://<ACCESS_TOKEN>@<admin-portal-host>`. **The access token is the URL user
name**, which is why the MCP server prints `THREESCALE_PORTAL_ENDPOINT` as
`https://[credentials redacted]@host`.

There is also an embedded-configuration mode
(`embeddedConfigurationSecretRef`) where the gateway serves a JSON
configuration from a secret and never talks to the admin portal at all. Such a
gateway will not appear in the Admin Portal analytics, and its APIs exist only
in that JSON.

## Consequences for troubleshooting

- **Always discover before assuming.** `3scale_list_apicast_gateways` sweeps the
  whole cluster: APIcast CRs, APIManager-managed gateways and any Deployment
  labelled or named `apicast`. It reports the namespace, the managing operator,
  readiness and the relevant `APICAST_*` settings for each.
- **API and gateway lookups are cluster-wide, always.** The reference topology
  is an APIManager in its own namespace and several self-managed gateways in
  others; that APIManager namespace holds no gateway and produces no traffic
  metrics. Searching for an API or a gateway is therefore never restricted by
  namespace — including not by `THREESCALE_NAMESPACE`, which identifies the API
  Manager rather than the gateways. Once an API is located, metric queries narrow
  to the namespaces actually serving it and the result names them. A `namespace`
  argument restricts deliberately, but if it matches nothing the tools widen the
  search and say so, rather than reporting an API as absent.
- **The same API can be served by several gateways.** The per-gateway table in
  `3scale_analyze_api_metrics` is what separates "the API is broken" from "one
  gateway is broken". Errors confined to a single namespace/deployment mean a
  gateway problem: stale configuration cache, a crashed pod, a different image
  version, or a network path that only that gateway uses.
- **Configuration drift between gateways is common.** Compare the per-gateway
  settings: different `APICAST_CONFIGURATION_CACHE`, a missing
  `APICAST_EXTENDED_METRICS`, or a different image tag explains why one gateway
  behaves differently from another serving the same API.
- **Monitoring is per namespace.** Each namespace holding gateways needs its own
  ServiceMonitor or PodMonitor; a gateway in a new namespace silently stops
  producing metrics until one is created there. That is the most common reason
  an API "disappears" from `3scale_list_apis` after a migration. A ServiceMonitor
  whose `port` does not exist on the gateway Service fails just as silently —
  `3scale_check_metrics_pipeline` checks for that specifically.
- **RBAC must be cluster-scoped.** The MCP server reads Deployments, pods, APIcast
  CRs, ServiceMonitors and metrics across namespaces; if a gateway namespace is
  missing from the report, suspect RBAC before concluding the gateway is absent.

## Checklist for a new self-managed gateway

1. `APIcast` CR created in the target namespace with valid admin portal
   credentials.
2. `extendedMetrics: true` so its traffic can be attributed to APIs.
3. A ServiceMonitor (or PodMonitor) in that namespace scraping port 9421.
4. Network path from the gateway to the API Manager (`system-master`/
   `backend-listener`) and to the upstream APIs.
5. Routes or an Ingress exposing the gateway on the hosts configured as public
   endpoints for the products it serves.
