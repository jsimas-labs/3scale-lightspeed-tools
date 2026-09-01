# Red Hat 3scale API Management on OpenShift — Architecture Overview

Red Hat 3scale API Management is deployed on OpenShift by the **3scale operator**
through the `APIManager` custom resource (`apps.3scale.net/v1alpha1`). All
components run in a single namespace (commonly `3scale`).

## Components and their pods

| Component | Pods / Deployments | Purpose |
|---|---|---|
| APIcast staging | `apicast-staging` | API gateway used to test configuration changes; picks up config every few seconds |
| APIcast production | `apicast-production` | API gateway serving production traffic; caches configuration (default 300s) |
| System | `system-app` | Rails application: Admin Portal, Developer Portal, Master Admin, internal APIs |
| System workers | `system-sidekiq` | Background jobs (emails, billing, webhooks, portal publishing) |
| System search | `system-searchd` | Full-text search index for the Admin/Developer Portal |
| Backend listener | `backend-listener` | Receives authorize/report traffic from APIcast (Service Management API) |
| Backend worker | `backend-worker` | Processes queued reports and analytics |
| Backend cron | `backend-cron` | Scheduled backend maintenance jobs |
| Zync | `zync`, `zync-que` | Synchronizes 3scale state to OpenShift Routes and to OpenID Connect providers (e.g. RH-SSO) |
| Zync database | `zync-database` | PostgreSQL for zync — the only database the operator may still deploy in-cluster |
| Memcached | `system-memcache` | System object cache |

From 3scale 2.15 all components run as Kubernetes **Deployments**
(earlier releases used DeploymentConfigs).

### External databases (required since 2.16)

Since 3scale 2.16 the operator **does not deploy** the Redis databases or the
system RDBMS. They are external, user-provided services declared in the
APIManager CR (`spec.externalComponents`) and configured via secrets:

| Database | Secret | Consumers |
|---|---|---|
| Backend Redis (storage + queues) | `backend-redis` | backend-listener/worker/cron |
| System Redis | `system-redis` | system-app, system-sidekiq |
| System RDBMS (MySQL/PostgreSQL/Oracle) | `system-database` | system-app, system-sidekiq |
| Zync PostgreSQL (if external) | `zync` | zync, zync-que |

Do not expect `backend-redis`/`system-redis`/`system-mysql` pods in the
namespace on 2.16; database troubleshooting starts from these secrets (see the
databases document).

## Key routes

Zync creates and maintains OpenShift Routes for:

- `<tenant>-admin.<wildcardDomain>` — Admin Portal (service `system-provider`)
- `master.<wildcardDomain>` — Master Admin Portal (service `system-master`)
- `<tenant>.<wildcardDomain>` — Developer Portal (service `system-developer`)
- `api-<tenant>-apicast-staging.<wildcardDomain>` — APIcast staging
- `api-<tenant>-apicast-production.<wildcardDomain>` — APIcast production

If routes are missing, the root cause is almost always in **zync**, not in the
router. See the zync troubleshooting document.

## Persistent storage

- `system-storage` PVC (RWX unless S3-compatible file storage is configured) — CMS assets
- `zync-database` PVC — only when zync's PostgreSQL is operator-deployed
- Database/Redis PVCs only exist on pre-2.16 or self-managed in-cluster databases

A `Pending` PVC blocks the corresponding pod in `Pending`/`ContainerCreating`
state. RWX storage (e.g. ODF `ocs-storagecluster-cephfs`, NFS) is required for
`system-storage` in multi-replica setups.

## Traffic flow (request path)

1. Client calls APIcast route → APIcast pod.
2. APIcast authorizes/report against `backend-listener` (service `backend-listener:3000`).
3. Backend checks credentials/limits in the (external) backend Redis and queues usage for `backend-worker`.
4. APIcast proxies the request to the upstream API (Private Base URL).

A failure in any of these hops surfaces as gateway errors: check APIcast logs
first, then backend-listener, then backend-redis connectivity.

## Gateway topology: APIcast is not confined to one namespace

Besides the `apicast-staging` and `apicast-production` Deployments that the
3scale operator creates next to the APIManager, the **APIcast operator** can
deploy self-managed gateways (kind `APIcast`, `apps.3scale.net/v1alpha1`) in any
namespace of the cluster — commonly one per team, environment or network zone.
Each of those is a Deployment named `apicast-<CR name>` in the namespace of its
custom resource, and each needs its own ServiceMonitor for metrics.

When troubleshooting, discover the gateways instead of assuming their location
(`3scale_list_apicast_gateways`), and remember that the same API can be served
by several gateways at once: an error confined to one namespace or deployment is
a gateway problem, not an API problem. See
`apicast-operator-multi-namespace.md`.

## Observability

APIcast exports Prometheus metrics on port 9421. With
`APICAST_EXTENDED_METRICS=true` the traffic metrics carry `service_id` and
`service_system_name` labels, which is what allows traffic, error rates and
latency to be analysed per API rather than per gateway. Collection on OpenShift
requires user workload monitoring to be enabled and a ServiceMonitor in each
namespace that hosts gateways. See `apicast-metrics-reference.md`.
