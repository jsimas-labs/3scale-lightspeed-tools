# 3scale Lightspeed Tools

Ferramentas do **OpenShift Lightspeed (OLS)** para diagnóstico do
**Red Hat 3scale API Management**, compostas por duas imagens publicáveis no
quay.io:

1. **Servidor MCP em Go** ([mcp-server/](mcp-server/)) — expõe ferramentas
   *read-only* de troubleshooting do 3scale via Model Context Protocol
   (transporte streamable HTTP), consumidas pelo OLS através de
   `spec.mcpServers`.
2. **Imagem BYOK** ([byok/](byok/)) — base de conhecimento RAG (FAISS) com
   runbooks de troubleshooting do 3scale, consumida pelo OLS através de
   `spec.ols.rag`.

```
┌────────────────────────── OpenShift ──────────────────────────┐
│  namespace openshift-lightspeed          namespace 3scale     │
│  ┌─────────────────────┐                ┌──────────────────┐  │
│  │ OLS (lightspeed-app) │── MCP/HTTP ──▶│ APIManager, pods, │  │
│  │  ├─ RAG: imagem BYOK │               │ routes, events…   │  │
│  │  └─ MCP: 3scale-     │◀── leitura ───┤ (via API do K8s)  │  │
│  │     troubleshoot     │               └──────────────────┘  │
│  └─────────────────────┘                                      │
└───────────────────────────────────────────────────────────────┘
```

## Ferramentas MCP disponíveis

| Ferramenta | Descrição |
|---|---|
| `diagnose_3scale` | Resumo de saúde em uma chamada: APIManager, deployments, pods, PVCs e eventos Warning |
| `get_apimanager_status` | Condições de status do CR APIManager (apps.3scale.net) |
| `list_3scale_pods` | Pods com fase, readiness, restarts e motivos de falha |
| `get_pod_logs` | Logs de um pod (container, tail, instância anterior/crashada) |
| `get_deployments` | Réplicas desejadas/prontas e condições não saudáveis |
| `get_events` | Eventos do namespace (opcionalmente só Warning), mais recentes primeiro |
| `check_routes` | Rotas (portais e gateways) com host, TLS e status de admissão |
| `check_database_config` | Configuração dos bancos a partir dos secrets (`backend-redis`, `system-redis`, `system-database`, `zync`) com credenciais censuradas, flags de `externalComponents` e teste opcional de conectividade TCP — essencial no 2.16, em que Redis e RDBMS são externos |
| `check_pvcs` | PVCs com fase, capacidade e storage class |

Todas as ferramentas aceitam `namespace` opcional; o default vem de
`THREESCALE_NAMESPACE` (padrão `3scale`).

## Pré-requisitos

- OpenShift 4.x com **OpenShift Lightspeed operator** instalado e OLSConfig
  `cluster` funcional (provider LLM configurado)
- 3scale API Management instalado via operator (CR `APIManager`)
- `podman`, `oc`, `make`, Go ≥ 1.24 (apenas para build local)
- Conta no [quay.io](https://quay.io) com um repositório **público** (ou pull
  secret configurado no cluster para repositórios privados)

---

## 1. Build e publicação do servidor MCP (quay.io)

```bash
podman login quay.io

# build + push (substitua CHANGE_ME pelo seu usuário/organização)
make image-build image-push REGISTRY_ORG=<seu-usuario>
# equivalente a:
#   podman build -t quay.io/<seu-usuario>/3scale-troubleshoot-mcp:latest \
#     -f mcp-server/Containerfile mcp-server/
#   podman push quay.io/<seu-usuario>/3scale-troubleshoot-mcp:latest
```

Build local do binário (desenvolvimento): `make build`. Para rodar localmente
contra o kubeconfig atual:

```bash
cd mcp-server && go run . -transport stdio -namespace 3scale
# ou HTTP: go run . -transport http -listen :8080  →  endpoint http://localhost:8080/mcp
```

## 2. Build e publicação da imagem BYOK (quay.io)

A imagem é gerada pela ferramenta oficial `lightspeed-rag-tool` a partir dos
markdowns de [byok/docs/](byok/docs/). Detalhes em [byok/README.md](byok/README.md).

```bash
podman login registry.redhat.io   # necessário para baixar a rag-tool
podman login quay.io

make byok-push REGISTRY_ORG=<seu-usuario>
# gera o índice FAISS, carrega byok-image.tar, tagueia e publica
# quay.io/<seu-usuario>/3scale-docs-byok:latest
```

## 3. Instalação no cluster

### 3.1 Deploy do servidor MCP

```bash
# RBAC (leitura de pods/logs/eventos/PVCs/deployments/routes/apimanagers)
# + Deployment/Service no namespace openshift-lightspeed
make deploy MCP_IMAGE=quay.io/<seu-usuario>/3scale-troubleshoot-mcp:latest

# se o 3scale não estiver no namespace "3scale", ajuste:
oc set env deployment/threescale-troubleshoot-mcp -n openshift-lightspeed \
  THREESCALE_NAMESPACE=<namespace-do-3scale>

# verificação
oc get pods -n openshift-lightspeed -l app=threescale-troubleshoot-mcp
oc exec -n openshift-lightspeed deploy/threescale-troubleshoot-mcp -- \
  /bin/sh -c 'true' 2>/dev/null || true
curl -s http://$(oc get svc threescale-troubleshoot-mcp -n openshift-lightspeed \
  -o jsonpath='{.spec.clusterIP}'):8080/healthz   # a partir de um pod do cluster
```

### 3.2 Configuração do OLSConfig

Edite o OLSConfig `cluster` (`oc edit olsconfig cluster`) e **mescle** os
campos abaixo — não substitua a configuração de provider existente. Exemplo
completo em [deploy/30-olsconfig-example.yaml](deploy/30-olsconfig-example.yaml):

```yaml
spec:
  featureGates:
    - MCPServer            # habilita a integração MCP (Tech Preview)
  mcpServers:
    - name: 3scale-troubleshoot
      url: "http://threescale-troubleshoot-mcp.openshift-lightspeed.svc.cluster.local:8080/mcp"
      timeout: 60
  ols:
    rag:
      - image: quay.io/<seu-usuario>/3scale-docs-byok:latest
        indexID: vector_db_index      # opcional (default)
        indexPath: /rag/vector_db     # opcional (default)
```

O operator do OLS reinicia o `lightspeed-app-server` aplicando o RAG e o MCP.

### 3.3 Verificação fim a fim

```bash
oc get pods -n openshift-lightspeed
oc logs deployment/lightspeed-app-server -n openshift-lightspeed | grep -i -e mcp -e rag
```

No console do OpenShift, abra o Lightspeed e pergunte, por exemplo:

- *"Diagnose my 3scale installation"* → deve acionar `diagnose_3scale`
- *"Why are my 3scale admin portal routes missing?"* → deve combinar o runbook
  de zync (RAG) com `check_routes`/`get_pod_logs` (MCP)

## Estrutura do repositório

```
├── mcp-server/          # servidor MCP em Go (main.go, k8s.go, tools.go, Containerfile)
├── byok/                # docs markdown + build.sh da imagem BYOK
│   └── docs/            # runbooks de troubleshooting do 3scale
├── deploy/              # RBAC, Deployment/Service, exemplo de OLSConfig
├── Makefile
└── README.md
```

## Segurança

- O servidor MCP é **somente leitura**: nenhuma ferramenta cria, altera ou
  apaga recursos (RBAC restrito a `get`/`list`).
- Secrets: o RBAC permite `get` apenas nos secrets de conexão de banco
  (`backend-redis`, `system-redis`, `system-database`, `zync`,
  `system-memcache`) — necessários porque desde o 3scale 2.16 os bancos são
  externos. Senhas **nunca** são retornadas: URLs são censuradas e chaves de
  senha aparecem apenas como `[set, redacted]`.
- Logs de pods podem conter dados sensíveis; o acesso ao OLS já exige
  autorização no cluster, mas restrinja o ClusterRole a namespaces específicos
  (troque por Role + RoleBinding) se necessário.
- Container roda como usuário não-root, rootfs read-only e sem capabilities.

## Referências

- [Bring your own knowledge to OpenShift Lightspeed (Red Hat Blog)](https://www.redhat.com/en/blog/bring-your-own-knowledge-openshift-lightspeed)
- [OpenShift Lightspeed — Configuração (docs oficiais)](https://docs.redhat.com/en/documentation/red_hat_openshift_lightspeed/1.0/html/configure/ols-configuring-openshift-lightspeed)
- [OLSConfig API reference](https://docs.redhat.com/en/documentation/red_hat_openshift_lightspeed/1.0/html/configure/olsconfig-api)
- [MCP Go SDK (oficial)](https://github.com/modelcontextprotocol/go-sdk)
- [3scale operator](https://github.com/3scale/3scale-operator)
