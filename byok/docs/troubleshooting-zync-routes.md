# Troubleshooting Zync and Missing OpenShift Routes

Zync synchronizes 3scale state to the outside world: it creates/updates
OpenShift **Routes** for portals and gateways, and synchronizes OIDC clients
to identity providers (e.g. Red Hat build of Keycloak / RH-SSO).
Pods: `zync` (API), `zync-que` (workers), `zync-database` (PostgreSQL).

## Symptom: routes missing after creating a product/tenant, or after backup-restore

Expected routes per tenant: admin portal, developer portal, master (cluster-wide),
apicast staging and production per gateway.

1. Check zync pods:
   ```bash
   oc get pods -n 3scale | grep zync
   oc logs deployment/zync-que -n 3scale --tail=200
   ```
2. Typical `zync-que` errors:
   - `403 Forbidden` from the Kubernetes API: the `zync` ServiceAccount lacks
     permissions on routes — verify role bindings created by the operator.
   - PG connection errors: `zync-database` down or PVC issues.
3. **Force a resync of all routes** (regenerates missing routes) by triggering
   a resync from system. On recent releases:
   ```bash
   oc exec deployment/system-app -c system-provider -n 3scale -- \
     bundle exec rake zync:resync:domains
   ```
   This re-enqueues domain/route sync jobs; watch `zync-que` logs and then
   `oc get routes -n 3scale`.
4. If specific routes exist but are wrong/stale, deleting the bad route and
   resyncing (previous step) recreates it.

## Symptom: route exists but shows "HostAlreadyClaimed" / not admitted

- Another route in the cluster already claims the same hostname. Find it:
  ```bash
  oc get routes -A --field-selector spec.host=<hostname>
  ```
- Remove or rename the conflicting route; router admits the 3scale route on
  the next sync.

## Symptom: OIDC clients not appearing in Keycloak/RH-SSO

- Zync creates one client per 3scale application when the product uses OIDC.
- Check the OIDC issuer URL configured in the product (must embed valid
  credentials of a client allowed to manage clients, e.g.
  `https://zync:<secret>@keycloak-host/realms/<realm>`).
- Inspect `zync-que` logs for HTTP errors against the IdP (401/403 mean wrong
  zync client credentials or missing service account roles in the realm).
- TLS: if the IdP uses a custom CA, zync must trust it; otherwise logs show
  `SSL_connect` errors. Recent releases read extra CAs from a mounted secret; check `oc describe deployment/zync-que -n 3scale` for the CA bundle mount and the APIManager zync options for the release in use.

## Symptom: zync-que jobs failing repeatedly / queue growing

- Inspect the que jobs table:
  ```bash
  oc rsh -n 3scale deployment/zync-database
  psql zync_production -c "select job_class, error_count, last_error_message from que_jobs order by error_count desc limit 10;"
  ```
- Address the underlying error (K8s RBAC, IdP connectivity), then jobs retry
  automatically with backoff.

## Useful checks

```bash
oc get routes -n 3scale
oc logs deployment/zync -n 3scale --tail=100
oc logs deployment/zync-que -n 3scale --tail=200
oc get events -n 3scale --field-selector type=Warning | grep -i zync
```
