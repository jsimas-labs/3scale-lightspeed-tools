# Troubleshooting 3scale Databases (2.16: external Redis and RDBMS)

**Important — since 3scale 2.16 the operator does not deploy or manage the
Redis databases.** Backend Redis (storage + queues), System Redis and usually
the System RDBMS (MySQL/PostgreSQL/Oracle) are **external** services provided
by the user (RHEL-hosted, Redis Enterprise/Cloud, RDS, etc.). The operator
only reads connection details from **secrets** and injects them into the
component Deployments. Only the zync PostgreSQL (`zync-database`) may still be
provisioned by the operator, unless declared external.

Never assume `backend-redis`, `system-redis` or `system-mysql` pods exist in
the namespace: **always start database troubleshooting from the secrets**.

## Where the connections are defined (secrets in the 3scale namespace)

| Secret | Keys (main) | Used by |
|---|---|---|
| `backend-redis` | `REDIS_STORAGE_URL`, `REDIS_QUEUES_URL`, `REDIS_STORAGE_SENTINEL_HOSTS`, `REDIS_QUEUES_SENTINEL_HOSTS`, `REDIS_STORAGE_SENTINEL_ROLE`, `REDIS_QUEUES_SENTINEL_ROLE`, ACL: `CONFIG_REDIS_USERNAME/PASSWORD`, `CONFIG_QUEUES_USERNAME/PASSWORD`, TLS: `REDIS_SSL_CA/CERT/KEY` | backend-listener, backend-worker, backend-cron |
| `system-redis` | `URL`, `SENTINEL_HOSTS`, `SENTINEL_ROLE`, `NAMESPACE`, ACL: `REDIS_USERNAME/PASSWORD`, TLS: `REDIS_SSL_CA/CERT/KEY` | system-app, system-sidekiq |
| `system-database` | `URL` (`mysql2://`, `postgresql://` or `oracle-enhanced://`), `DB_USER`, `DB_PASSWORD`, TLS: `DATABASE_SSL_MODE`, `DB_SSL_CA/CERT/KEY` | system-app, system-sidekiq |
| `zync` | `DATABASE_URL`, `ZYNC_DATABASE_PASSWORD`, `SECRET_KEY_BASE`, TLS: `DATABASE_SSL_MODE`, `DB_SSL_CA` | zync, zync-que |

Which databases are external is declared in the APIManager CR:

```yaml
spec:
  externalComponents:
    backend:
      redis: true
    system:
      database: true
      redis: true
    # zync.database: true when zync DB is also external
```

Inspect a secret safely (values are base64; avoid printing passwords):

```bash
oc get secret backend-redis -n 3scale -o jsonpath='{.data.REDIS_STORAGE_URL}' | base64 -d
oc get secret system-database -n 3scale -o jsonpath='{.data.URL}' | base64 -d
```

## Symptom: many pods failing at once (system-app, sidekiq, backend-worker)

A shared external dependency is down or unreachable. Identify the endpoint
from the secrets above, then test connectivity **from inside the namespace**
(node-level tests do not prove pod-level egress):

```bash
HOST=redis.example.com PORT=6379
oc exec deployment/backend-listener -n 3scale -- \
  bash -c "(echo > /dev/tcp/$HOST/$PORT) && echo OK || echo FAIL"
```

If TCP fails, check: DNS resolution in the pod, NetworkPolicies/egress
firewall, security groups on the database side, and TLS-only listeners being
addressed with `redis://` instead of `rediss://`.

## Redis-specific errors in component logs

- **`NOAUTH Authentication required` / `WRONGPASS`** — ACL credentials in the
  secret don't match the server. Fix the secret; the operator re-rolls the
  deployments (or `oc rollout restart` the consumers).
- **`LOADING Redis is loading the dataset in memory`** — server restarting
  with a big dataset; dependent pods crashloop until it finishes.
- **`READONLY You can't write against a read only replica`** — clients are
  pointed at a replica: with Sentinel, check `*_SENTINEL_ROLE: master` and the
  sentinel hosts list; without Sentinel, point URLs at the master.
- **`OOM command not allowed when used memory > 'maxmemory'`** — external
  Redis is full: grow it or review eviction policy/analytics retention.
- **TLS handshake errors** — URL scheme must be `rediss://` and the CA in
  `REDIS_SSL_CA` must match the server certificate.

Run redis-cli against the external endpoint from a throwaway pod:

```bash
oc run redis-check --rm -it --restart=Never -n 3scale \
  --image=registry.redhat.io/rhel9/redis-7 -- \
  redis-cli -u "$(oc get secret backend-redis -n 3scale -o jsonpath='{.data.REDIS_STORAGE_URL}' | base64 -d)" ping
```

## System database (external MySQL/PostgreSQL/Oracle)

- Connection errors in system-app (`Mysql2::Error: Can't connect`,
  `PG::ConnectionBad`): validate `system-database` secret `URL`, credentials
  and reachability from the namespace (test as above with port 3306/5432).
- **Migrations pending** after upgrade: system-app readiness fails, log shows
  `Migrations are pending`. Check the operator logs and the system-app
  pre-hook pod that runs migrations.
- Requirements (2.16): MySQL 8.x / PostgreSQL 13+ (check the release's
  supported configurations page for exact versions), UTF-8 encoding.

## Zync database

- `zync-database` is the one database the operator may still deploy in-cluster.
  If internal: check the pod and its PVC. If external
  (`externalComponents.zync.database: true`): validate the `zync` secret
  `DATABASE_URL`.
- Zync failures surface as missing routes — see the zync document.

## Memcached

- `system-memcache` remains an in-cluster Deployment. It is stateless;
  restart is safe: `oc rollout restart deployment/system-memcache -n 3scale`.

## Backup reminders

- The system database, both backend Redis (storage/queues), system Redis and
  the `system-storage` assets represent the state of 3scale. Back up and
  restore them **consistently as a set** — they are external now, so include
  them in your database backup tooling, not in cluster backups.
