# BYOK image — 3scale troubleshooting documentation

This directory builds a **BYOK (Bring Your Own Knowledge)** image for
OpenShift Lightspeed containing a RAG knowledge base (FAISS) built from the
3scale troubleshooting runbooks in [docs/](docs/).

## Contents

| File | Topic |
|---|---|
| `docs/3scale-architecture-overview.md` | Architecture, components, routes, traffic flow, gateway topology and observability |
| `docs/mcp-tools-playbook.md` | Which MCP tool to call for which symptom, and how to read its output |
| `docs/apicast-metrics-reference.md` | APIcast Prometheus metrics, their labels, and thresholds for interpreting them |
| `docs/troubleshooting-api-metrics.md` | Metric-signature runbooks: 502/503/504/499/403/404/429, latency, traffic loss |
| `docs/apicast-operator-multi-namespace.md` | Self-managed gateways deployed by the APIcast operator in any namespace |
| `docs/troubleshooting-apicast.md` | Gateway: 403/404/502, CrashLoop, configuration cache |
| `docs/troubleshooting-system.md` | Admin/Developer Portal, sidekiq, searchd, credentials |
| `docs/troubleshooting-backend.md` | listener/worker/cron, analytics, resque queues |
| `docs/troubleshooting-zync-routes.md` | Missing routes, resync, OIDC/Keycloak |
| `docs/troubleshooting-databases.md` | External databases (2.16): connection secrets, Redis, MySQL/PostgreSQL, memcached |
| `docs/troubleshooting-operator-apimanager.md` | Operator, APIManager CR, upgrades, must-gather |
| `docs/troubleshooting-certificates-networking.md` | TLS, certificates, DNS, proxies |

The four documents on MCP tools, metrics and gateway topology exist to make the model effective **with the MCP server**:
they explain what each tool returns, what the metric names and labels mean, and
how to turn a status-code distribution or a latency split into a root cause. The
knowledge base and the MCP server are designed to be deployed together.

You can add more `.md` files under `docs/` (including subdirectories) —
for example, official 3scale documentation converted to markdown — and
rebuild the image.

## Build

[build.sh](build.sh) builds the image in **two decoupled stages**:

1. **FAISS index** — runs `generate_embeddings_tool.py` from the official
   `lightspeed-rag-tool-rhel9` tool (Python only) to generate `vector_db` from
   the markdowns in `docs/`.
2. **Final image** — packages `vector_db` into a minimal UBI base with
   `podman build --platform linux/amd64` ([Containerfile](Containerfile)).

```bash
# 1. log in to the registries
podman login registry.redhat.io   # to pull the rag-tool
podman login quay.io

# 2. build + push (amd64 image by default)
IMAGE=quay.io/<your-user>/3scale-docs-byok:latest ./build.sh --push
```

Useful environment variables: `PLATFORM` (default `linux/amd64`), `INDEX_ID`,
`EMBEDDING_MODEL`, `RAG_TOOL_IMAGE`, `DOCS_DIR`, `OUTPUT_DIR`.

### Why not use the rag-tool default mode?

The default `lightspeed-rag-tool` mode runs a **nested** `buildah build` to
produce the image. On **arm64 (Apple Silicon)** hosts, that nested buildah
fails under amd64 emulation with `Error during reexec: No such file or
directory`. Running only the Python stage avoids nested buildah, and
`podman build --platform linux/amd64` ensures the final image matches the
architecture of the OpenShift nodes, regardless of the build host.

> On an arm64 host, the Python stage runs emulated (amd64). It is fast for a
> small number of documents. Verify that emulation is active:
> `podman run --rm --platform linux/amd64 registry.access.redhat.com/ubi9/ubi-micro uname -m`
> should print `x86_64`. If not, install the emulators:
> `podman machine ssh sudo rpm-ostree install qemu-user-static` (or use an
> amd64 `podman machine` / an x86_64 host).

## Usage with OpenShift Lightspeed

Add to the OLSConfig `cluster` (under `spec.ols`, at the same level as `defaultModel`):

```yaml
spec:
  ols:
    rag:
      - image: quay.io/<your-user>/3scale-docs-byok:latest
        indexID: vector_db_index
        indexPath: /rag/vector_db
```

`indexID` and `indexPath` are optional (the values above are the defaults).
The operator restarts the `lightspeed-app-server` pod and mounts the index from the image.

> Note: if the quay.io repository is private, add the pull secret to the
> OLS service account or make the repository public.

## Verification

```bash
# confirm architecture (must be amd64 for OpenShift)
podman image inspect quay.io/<your-user>/3scale-docs-byok:latest \
  --format '{{.Os}}/{{.Architecture}}'

# inspect the generated image contents
podman create --replace --name tmp-rag quay.io/<your-user>/3scale-docs-byok:latest true
podman cp tmp-rag:/rag/vector_db ./vector_db-inspect
podman rm tmp-rag
```

After applying the OLSConfig, ask Lightspeed something like
*"why are my 3scale routes missing?"* — the answer should reflect the runbook
content (e.g. check zync-que and run `zync:resync:domains`).

With the MCP server deployed as well, ask something like *"how is the Echo API
doing in the last hour?"* or *"why is my API returning 502?"*: Lightspeed should
call `3scale_analyze_api_metrics`, then explain the status-code distribution and
the latency split using the vocabulary from these documents.
