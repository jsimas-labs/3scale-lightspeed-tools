# Ajuste REGISTRY_ORG para o seu usuário/organização no quay.io
REGISTRY      ?= quay.io
REGISTRY_ORG  ?= CHANGE_ME
MCP_IMAGE     ?= $(REGISTRY)/$(REGISTRY_ORG)/3scale-troubleshoot-mcp:latest
BYOK_IMAGE    ?= $(REGISTRY)/$(REGISTRY_ORG)/3scale-docs-byok:latest

.PHONY: build test image-build image-push byok-build byok-push deploy undeploy

build: ## compila o binário do MCP server
	cd mcp-server && go build -o bin/3scale-troubleshoot-mcp .

test: ## vet + build
	cd mcp-server && go vet ./... && go build ./...

image-build: ## constrói a imagem do MCP server
	podman build -t $(MCP_IMAGE) -f mcp-server/Containerfile mcp-server/

image-push: ## publica a imagem do MCP server no registry
	podman push $(MCP_IMAGE)

byok-build: ## constrói a imagem BYOK com a documentação do 3scale
	IMAGE=$(BYOK_IMAGE) ./byok/build.sh

byok-push: ## constrói e publica a imagem BYOK
	IMAGE=$(BYOK_IMAGE) ./byok/build.sh --push

deploy: ## aplica RBAC, Deployment e Service do MCP server (namespace openshift-lightspeed)
	oc apply -f deploy/10-rbac.yaml
	sed 's|quay.io/CHANGE_ME/3scale-troubleshoot-mcp:latest|$(MCP_IMAGE)|' deploy/20-deployment.yaml | oc apply -f -

undeploy:
	oc delete -f deploy/20-deployment.yaml --ignore-not-found
	oc delete -f deploy/10-rbac.yaml --ignore-not-found
