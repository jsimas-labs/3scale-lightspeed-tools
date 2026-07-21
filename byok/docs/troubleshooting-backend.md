# Troubleshooting 3scale Backend (listener, worker, cron, analytics)

Backend implements the Service Management API used by APIcast for
authorize/report calls, and processes usage analytics. Pods:
`backend-listener`, `backend-worker`, `backend-cron`.

Storage: **external** backend Redis (storage + queues), configured in the
`backend-redis` secret (`REDIS_STORAGE_URL`, `REDIS_QUEUES_URL`). Since 3scale
2.16 there is no backend-redis pod in the namespace. Get the endpoint with:

```bash
oc get secret backend-redis -n 3scale -o jsonpath='{.data.REDIS_QUEUES_URL}' | base64 -d
```

## Symptom: APIcast returns 403/timeout even with valid credentials

- `backend-listener` may be down or unreachable:
  ```bash
  oc get pods -n 3scale | grep backend
  oc logs deployment/backend-listener -n 3scale --tail=100
  ```
- Test the internal status endpoint:
  ```bash
  oc exec deployment/apicast-production -n 3scale -- \
    curl -s http://backend-listener:3000/status
  ```
  Expected: `{"status":"ok"}`.
- If listener logs show Redis errors (`ECONNREFUSED`, `NOAUTH`, `LOADING`,
  timeouts), troubleshoot the external backend Redis via the `backend-redis`
  secret (see databases document).

## Symptom: analytics/usage numbers missing or delayed in the Admin Portal

- Usage reports are queued in the backend Redis (queues) and drained by
  `backend-worker`. If workers are down or crashing, traffic still flows
  (authorization is cached) but analytics lag.
- Check worker health and logs:
  ```bash
  oc logs deployment/backend-worker -n 3scale --tail=100
  ```
- Inspect the resque queue backlog on the external queues Redis:
  ```bash
  QURL=$(oc get secret backend-redis -n 3scale -o jsonpath='{.data.REDIS_QUEUES_URL}' | base64 -d)
  oc run redis-check --rm -it --restart=Never -n 3scale \
    --image=registry.redhat.io/rhel9/redis-7 -- redis-cli -u "$QURL" llen resque:queue:priority
  ```
  A continuously growing queue means workers cannot keep up or are failing —
  scale `backend-worker` or fix the errors in its log.

## Symptom: backend-worker CrashLoopBackOff after Redis restore/upgrade

- Workers fail fast when the external Redis is unreachable, rejects the ACL
  credentials (`NOAUTH`/`WRONGPASS` — check the `backend-redis` secret) or is
  still loading a large RDB/AOF (`LOADING Redis is loading the dataset in
  memory`). Check on the Redis server side, or remotely:
  ```bash
  oc run redis-check --rm -it --restart=Never -n 3scale \
    --image=registry.redhat.io/rhel9/redis-7 -- redis-cli -u "$QURL" info persistence
  ```

## Symptom: rate limits not being enforced (or enforced wrongly)

- Limits are evaluated by backend against counters in the backend storage
  Redis. Check that the application plan actually has limits defined and the
  metric mapping matches the mapping rules hit by traffic.
- APIcast caches authorizations briefly; small overshoot over the limit is
  expected behavior (asynchronous reporting).

## Useful checks

```bash
# Backend internal API status via the listener service
oc exec deployment/backend-listener -n 3scale -- curl -s localhost:3000/status

# Redis connectivity from backend pods (host/port from the backend-redis secret)
oc exec deployment/backend-listener -n 3scale -- \
  bash -c 'echo > /dev/tcp/<redis-host>/<redis-port> && echo redis reachable'

# Error queue size (failed jobs) on the external queues Redis
oc run redis-check --rm -it --restart=Never -n 3scale \
  --image=registry.redhat.io/rhel9/redis-7 -- redis-cli -u "$QURL" llen resque:failed
```
