#!/usr/bin/env bash
# Constrói a imagem BYOK (Bring Your Own Knowledge) do OpenShift Lightspeed
# a partir dos markdowns em byok/docs/.
#
# Abordagem desacoplada em duas etapas:
#   1. Gera o índice FAISS (vector_db) rodando generate_embeddings_tool.py da
#      lightspeed-rag-tool diretamente (só Python).
#   2. Empacota o vector_db numa imagem UBI mínima com `podman build`.
#
# Motivo: a lightspeed-rag-tool, no modo padrão, executa um `buildah build`
# ANINHADO que falha sob emulação amd64 em hosts arm64
# ("Error during reexec: No such file or directory"). Rodar só o Python evita
# o buildah aninhado, e o `podman build --platform` garante a arquitetura da
# imagem final (amd64 para os nós do OpenShift), independente do host.
#
# Pré-requisitos:
#   - podman logado em registry.redhat.io (para baixar a rag-tool)
#   - podman logado no registry de destino (ex.: quay.io) para o push
#   - em host arm64 (Apple Silicon): emulação amd64 disponível
#     (`podman machine` recente já inclui; senão veja o README)
#
# Uso:
#   ./build.sh                                   # gera e constrói a imagem local (amd64)
#   IMAGE=quay.io/meuuser/3scale-docs-byok:latest ./build.sh --push
#   PLATFORM=linux/arm64 ./build.sh              # sobrescreve a plataforma
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DOCS_DIR="${DOCS_DIR:-$SCRIPT_DIR/docs}"
OUTPUT_DIR="${OUTPUT_DIR:-$SCRIPT_DIR/output}"
RAG_TOOL_IMAGE="${RAG_TOOL_IMAGE:-registry.redhat.io/openshift-lightspeed-tech-preview/lightspeed-rag-tool-rhel9:latest}"
IMAGE="${IMAGE:-quay.io/CHANGE_ME/3scale-docs-byok:latest}"
PLATFORM="${PLATFORM:-linux/amd64}"
INDEX_ID="${INDEX_ID:-vector_db_index}"
EMBEDDING_MODEL="${EMBEDDING_MODEL:-sentence-transformers/all-mpnet-base-v2}"

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

echo ">> [1/2] Gerando índice FAISS a partir de: $DOCS_DIR (plataforma $PLATFORM)"
# CWD /output + caminho de saída relativo: o script sanitiza o -o removendo a
# barra inicial, então o vector_db é gravado relativo ao diretório atual.
podman run --rm --platform "$PLATFORM" --user 0 \
  -v "$DOCS_DIR:/markdown:Z" \
  -v "$OUTPUT_DIR:/output:Z" \
  --entrypoint /bin/sh \
  "$RAG_TOOL_IMAGE" \
  -c "cd /output && python3.12 /workdir/generate_embeddings_tool.py \
        -i /markdown -emd /workdir/embeddings_model -emn '$EMBEDDING_MODEL' \
        -o vector_db -id '$INDEX_ID'"

if [[ ! -f "$OUTPUT_DIR/vector_db/index_store.json" ]]; then
  echo "ERRO: índice não gerado em $OUTPUT_DIR/vector_db" >&2
  exit 1
fi
echo ">> Índice gerado: $(ls "$OUTPUT_DIR/vector_db" | tr '\n' ' ')"

echo ">> [2/2] Construindo imagem $IMAGE ($PLATFORM)"
podman build --platform "$PLATFORM" -t "$IMAGE" -f "$SCRIPT_DIR/Containerfile" "$SCRIPT_DIR"

echo ">> Arquitetura da imagem: $(podman image inspect "$IMAGE" --format '{{.Os}}/{{.Architecture}}')"

if [[ "${1:-}" == "--push" ]]; then
  echo ">> Publicando $IMAGE"
  podman push "$IMAGE"
else
  echo ">> Push não solicitado. Para publicar: podman push $IMAGE"
fi

echo ">> Concluído."
