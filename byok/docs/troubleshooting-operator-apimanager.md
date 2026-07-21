# Troubleshooting the 3scale Operator and the APIManager CR

The 3scale operator (installed via OLM, package `3scale-operator`) reconciles
the `APIManager` custom resource and deploys/updates all components.

## First checks

```bash
# Operator health
oc get csv -n 3scale | grep 3scale
oc get pods -n 3scale | grep 3scale-operator
oc logs deployment/threescale-operator-controller-manager-v2 -n 3scale --tail=200

# APIManager status
oc get apimanager -n 3scale
oc get apimanager <name> -n 3scale -o jsonpath='{.status.conditions}' | jq .
```

An healthy APIManager reports the `Available` condition with status `"True"`.

## Symptom: APIManager Available=False and components not coming up

- Read the operator log: it prints the exact reconcile error (missing secret,
  invalid field combination, image pull problems, PVC creation failures).
- Common causes:
  - **Missing database secrets** — since 2.16 the Redis databases (and usually
    the system RDBMS) are external and the `backend-redis`, `system-redis` and
    `system-database` secrets must exist **before** creating the APIManager
    with matching `spec.externalComponents` flags. The operator log reports
    which secret/key is missing.
  - **Invalid wildcardDomain** or DNS not resolving to the router.
  - Missing/renamed secrets referenced by the CR (e.g. S3 credentials in
    `spec.system.fileStorage`).
  - ResourceQuota/LimitRange in the namespace preventing pod creation —
    check `oc get events -n 3scale --field-selector type=Warning`.

## Symptom: upgrade stuck (CSV in Installing/Failed, or pods on old version)

1. Check the subscription and installplan:
   ```bash
   oc get subscription,installplan -n 3scale
   oc describe csv <3scale-csv> -n 3scale | tail -30
   ```
2. Operator upgrades proceed component by component; a component that cannot
   roll out (failing probes, Pending pods) blocks completion. Find it with
   `oc get deployments -n 3scale` (READY column) and fix the underlying issue.
3. 3scale upgrades are sequential: you cannot skip minor versions. Confirm the
   current version annotation:
   `oc get apimanager <name> -n 3scale -o jsonpath='{.metadata.annotations.apps\.3scale\.net/apimanager-threescale-version}'`.
4. **Upgrading to 2.16 requires external databases**: operator-managed Redis
   (and databases) must be migrated to external instances and the
   `backend-redis`/`system-redis`/`system-database` secrets pointed at them
   (with `spec.externalComponents` set) **before** the upgrade; otherwise the
   operator blocks or the components fail to start. Follow the 2.15 → 2.16
   migration guide.

## Symptom: change made in the APIManager CR has no effect

- Confirm the field is actually supported in the installed version
  (`oc explain apimanager.spec...`).
- The operator reverts manual edits on managed Deployments — always change
  the CR, not the Deployment.
- Watch the operator log while saving the CR to see the reconcile happening.

## Symptom: pods keep being recreated / config reverted

That is the operator enforcing the CR (expected). To customize resources,
replicas or affinity, set them in the APIManager CR
(`spec.<component>.resources`, `spec.<component>.replicas`, etc.).

## Collecting data for Red Hat support

```bash
# 3scale-specific must-gather (adjust image to your 3scale version)
oc adm must-gather --image=registry.redhat.io/3scale-amp2/3scale-must-gather-rhel9:3scale<version> --dest-dir=3scale-mg
```
Attach also `oc get apimanager -o yaml` and operator logs.
