# Imagem BYOK — Documentação de troubleshooting do 3scale

Este diretório gera uma imagem **BYOK (Bring Your Own Knowledge)** para o
OpenShift Lightspeed contendo uma base RAG (FAISS) construída a partir dos
runbooks de troubleshooting do 3scale em [docs/](docs/).

## Conteúdo

| Arquivo | Tema |
|---|---|
| `docs/3scale-architecture-overview.md` | Arquitetura, componentes, rotas e fluxo de tráfego |
| `docs/troubleshooting-apicast.md` | Gateway: 403/404/502, CrashLoop, cache de configuração |
| `docs/troubleshooting-system.md` | Admin/Developer Portal, sidekiq, searchd, credenciais |
| `docs/troubleshooting-backend.md` | listener/worker/cron, analytics, filas resque |
| `docs/troubleshooting-zync-routes.md` | Rotas ausentes, resync, OIDC/Keycloak |
| `docs/troubleshooting-databases.md` | Bancos externos (2.16): secrets de conexão, Redis, MySQL/PostgreSQL, memcached |
| `docs/troubleshooting-operator-apimanager.md` | Operator, APIManager CR, upgrades, must-gather |
| `docs/troubleshooting-certificates-networking.md` | TLS, certificados, DNS, proxies |

Você pode adicionar mais arquivos `.md` em `docs/` (inclusive subdiretórios) —
por exemplo, a documentação oficial do 3scale convertida para markdown — e
regerar a imagem.

## Build

O [build.sh](build.sh) constrói a imagem em **duas etapas desacopladas**:

1. **Índice FAISS** — roda `generate_embeddings_tool.py` da ferramenta oficial
   `lightspeed-rag-tool-rhel9` (só Python) para gerar o `vector_db` a partir
   dos markdowns em `docs/`.
2. **Imagem final** — empacota o `vector_db` numa base UBI mínima com
   `podman build --platform linux/amd64` ([Containerfile](Containerfile)).

```bash
# 1. login nos registries
podman login registry.redhat.io   # para baixar a rag-tool
podman login quay.io

# 2. build + push (imagem amd64 por padrão)
IMAGE=quay.io/<seu-usuario>/3scale-docs-byok:latest ./build.sh --push
```

Variáveis de ambiente úteis: `PLATFORM` (default `linux/amd64`), `INDEX_ID`,
`EMBEDDING_MODEL`, `RAG_TOOL_IMAGE`, `DOCS_DIR`, `OUTPUT_DIR`.

### Por que não usar o modo padrão da rag-tool?

O modo padrão da `lightspeed-rag-tool` executa um `buildah build` **aninhado**
para produzir a imagem. Em hosts **arm64 (Apple Silicon)**, esse buildah
aninhado falha sob emulação amd64 com `Error during reexec: No such file or
directory`. Rodar apenas a etapa Python evita o buildah aninhado, e o
`podman build --platform linux/amd64` garante que a imagem final tenha a
arquitetura dos nós do OpenShift, independentemente do host de build.

> Em host arm64, a etapa Python roda emulada (amd64). É rápida para poucos
> documentos. Verifique que a emulação está ativa:
> `podman run --rm --platform linux/amd64 registry.access.redhat.com/ubi9/ubi-micro uname -m`
> deve imprimir `x86_64`. Se não, instale os emuladores:
> `podman machine ssh sudo rpm-ostree install qemu-user-static` (ou use uma
> `podman machine` amd64 / um host x86_64).

## Uso no OpenShift Lightspeed

Adicione ao OLSConfig `cluster` (em `spec.ols`, mesmo nível de `defaultModel`):

```yaml
spec:
  ols:
    rag:
      - image: quay.io/<seu-usuario>/3scale-docs-byok:latest
        indexID: vector_db_index
        indexPath: /rag/vector_db
```

`indexID` e `indexPath` são opcionais (os valores acima são os defaults).
O operator reinicia o pod `lightspeed-app-server` e monta o índice da imagem.

> Nota: se o repositório no quay.io for privado, adicione o pull secret ao
> service account do OLS ou torne o repositório público.

## Verificação

```bash
# confirmar a arquitetura (deve ser amd64 para o OpenShift)
podman image inspect quay.io/<seu-usuario>/3scale-docs-byok:latest \
  --format '{{.Os}}/{{.Architecture}}'

# inspecionar o conteúdo da imagem gerada
podman create --replace --name tmp-rag quay.io/<seu-usuario>/3scale-docs-byok:latest true
podman cp tmp-rag:/rag/vector_db ./vector_db-inspect
podman rm tmp-rag
```

Depois de aplicado o OLSConfig, pergunte ao Lightspeed algo como
*"why are my 3scale routes missing?"* — a resposta deve refletir o conteúdo
dos runbooks (ex.: verificar zync-que e rodar `zync:resync:domains`).
