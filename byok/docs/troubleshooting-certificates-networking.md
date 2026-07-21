# Troubleshooting 3scale TLS, Certificates and Networking

## Symptom: browser/client reports invalid certificate on 3scale routes

- 3scale routes use **edge termination** with the OpenShift router certificate
  by default. If the wildcard router certificate does not cover
  `*.<wildcardDomain>` (e.g. 3scale uses a different subdomain than
  `*.apps...`), clients see certificate mismatch.
- Options: use a wildcardDomain under the router's certificate, replace the
  router certificate, or set per-route certificates (edit route TLS via zync
  is not persistent — routes are recreated by zync; prefer router-level or
  APIManager-supported certificate configuration).

## Symptom: APIcast cannot call HTTPS upstream (Private Base URL)

- Logs show `upstream SSL certificate verify error` or `handshake failed`.
- The upstream certificate must be trusted by APIcast. For custom CAs, mount
  the CA and point `SSL_CERT_FILE` to it, or (per release) use the APIcast
  custom CA options in the APIManager CR / APIcast policies
  (`upstream_mtls` for mutual TLS).
- For quick isolation, test from the pod:
  ```bash
  oc exec deployment/apicast-production -n 3scale -- curl -sv https://<upstream> 2>&1 | grep -i cert
  ```

## Symptom: OIDC integration fails with TLS errors

- Both APIcast (token validation) and zync (client sync) must trust the IdP
  certificate. Self-signed IdP certs cause `SSL_connect returned=1` in zync
  logs and JWT validation failures in APIcast.
- Provide the CA bundle to both components (APIManager options for custom CA
  secrets on recent releases).

## Symptom: intermittent timeouts between components

- Check for NetworkPolicies in the namespace that may block intra-namespace
  traffic: `oc get networkpolicy -n 3scale`.
- Check cluster DNS health (CoreDNS) — component discovery is by service name.
- Egress: if the cluster uses an egress firewall/proxy, APIcast → external
  upstreams and zync → external IdP need explicit allowance. For proxies,
  APIcast supports `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` (and per-service
  proxy policies).

## Symptom: 3scale unreachable from outside but pods healthy

1. Routes admitted? `oc get routes -n 3scale` and see zync document if missing.
2. DNS: `*.<wildcardDomain>` must resolve to the router/load balancer:
   `dig +short <tenant>-admin.<wildcardDomain>`.
3. Router logs: `oc logs -n openshift-ingress deploy/router-default | grep <host>`.
4. Test bypassing DNS:
   `curl -sk https://<router-IP> -H "Host: <tenant>-admin.<wildcardDomain>"`.

## Hostname rewriting / custom domains for APIs

- Use the APIcast **routing/host header policies** when the upstream requires
  a specific Host header; misconfigured host rewriting commonly shows as 404
  from the upstream while APIcast reports 200 on authorization.
