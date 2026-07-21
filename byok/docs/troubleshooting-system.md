# Troubleshooting 3scale System (Admin Portal, Developer Portal, Sidekiq)

System is the Rails application behind the Admin Portal, Master Portal and
Developer Portal. Pods: `system-app` (containers: `system-master`,
`system-provider`, `system-developer`), `system-sidekiq`, `system-searchd`.

## Symptom: Admin Portal returns 503 or does not load

1. Check `system-app` readiness:
   `oc get pods -n 3scale -l 'deployment in (system-app)'` — all containers must be ready.
2. `system-app` depends on: the external system database (secret
   `system-database`), the external system Redis (secret `system-redis`),
   `system-memcache` and the `system-storage` PVC. Since 3scale 2.16 the
   database and Redis are external services — if any of them is down or
   unreachable, system-app fails readiness. Check them first (see the
   databases document).
3. Check logs per container:
   ```bash
   oc logs deployment/system-app -c system-provider -n 3scale --tail=200
   ```
   Look for database connection errors (`Mysql2::Error`, `PG::ConnectionBad`)
   or Redis errors (`Redis::CannotConnectError`).
4. Route problems: confirm the `system-provider` route exists and is admitted
   (`oc get routes -n 3scale`). Missing route → see zync troubleshooting.

## Symptom: system-app pods stuck in Pending / ContainerCreating

- Usually the `system-storage` PVC is not Bound. This PVC requires **RWX**
  (ReadWriteMany) storage unless S3-compatible file storage is configured in
  the APIManager CR. Check: `oc get pvc -n 3scale`.
- If no RWX storage class exists, configure S3 file storage
  (`spec.system.fileStorage.simpleStorageService`) instead.

## Symptom: emails, webhooks or portal changes not applied (background jobs stuck)

- Background work is processed by `system-sidekiq`. Check the pod and its logs.
- Sidekiq depends on the external system Redis (secret `system-redis`, key
  `URL`); connection errors there stall all jobs.
- Job backlog inspection: Admin Portal (Master) → Sidekiq web UI, or check
  queue sizes on the external system Redis:
  ```bash
  SURL=$(oc get secret system-redis -n 3scale -o jsonpath='{.data.URL}' | base64 -d)
  oc run redis-check --rm -it --restart=Never -n 3scale \
    --image=registry.redhat.io/rhel9/redis-7 -- redis-cli -u "$SURL" llen queue:default
  ```

## Symptom: searching in the Admin/Developer Portal returns nothing

- `system-searchd` maintains the search index. Restart it to force reindex:
  `oc rollout restart deployment/system-searchd -n 3scale`.
- Reindex job can also be triggered from a system-app pod:
  `oc exec deployment/system-app -c system-provider -n 3scale -- bundle exec rake searchd:enqueue_reindex` (release-dependent; verify the rake task available with `rake -T | grep -i search`).

## Symptom: lost admin password / need admin access

- Credentials are stored in secrets:
  - `system-seed`: `ADMIN_USER`, `ADMIN_PASSWORD` (tenant admin), `MASTER_USER`,
    `MASTER_PASSWORD`, `MASTER_ACCESS_TOKEN`.
  ```bash
  oc get secret system-seed -n 3scale -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d
  ```

## Useful checks

```bash
# All system pods at a glance
oc get pods -n 3scale | grep system

# Rails console (advanced, read-only investigation)
oc exec -it deployment/system-app -c system-provider -n 3scale -- bundle exec rails console

# Database connectivity from system-app (host/port from the system-database secret URL)
oc get secret system-database -n 3scale -o jsonpath='{.data.URL}' | base64 -d  # note host and port
oc exec deployment/system-app -c system-provider -n 3scale -- \
  bash -c 'echo > /dev/tcp/<db-host>/<db-port> && echo DB reachable'
```
