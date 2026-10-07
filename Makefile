# Image used by docker-build, docker-push, build-installer and deploy.
IMG ?= ghcr.io/rijngshaffen/webapp-operator:latest
# Version compiled into the binary; reported at startup and by --version.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Platforms built by docker-buildx.
PLATFORMS ?= linux/amd64,linux/arm64
# Any OCI-compatible builder works here (docker, podman, nerdctl).
CONTAINER_TOOL ?= docker
# Kubernetes version of the control plane binaries used by the tests.
ENVTEST_K8S_VERSION ?= 1.37

# Tools are installed into ./bin at pinned versions so builds are reproducible
# and nothing is installed globally.
LOCALBIN := $(CURDIR)/bin
CONTROLLER_GEN := $(LOCALBIN)/controller-gen
KUSTOMIZE := $(LOCALBIN)/kustomize
SETUP_ENVTEST := $(LOCALBIN)/setup-envtest
GOLANGCI_LINT := $(LOCALBIN)/golangci-lint
GOVULNCHECK := $(LOCALBIN)/govulncheck

CONTROLLER_TOOLS_VERSION ?= v0.22.0
KUSTOMIZE_VERSION ?= v5.8.2
SETUP_ENVTEST_VERSION ?= v0.25.2
GOLANGCI_LINT_VERSION ?= v2.14.0
GOVULNCHECK_VERSION ?= v1.8.0

.DEFAULT_GOAL := build

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)}' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate the CRD and the operator's ClusterRole from the Go markers.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate DeepCopy methods for the API types.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./..."

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint.
	$(GOLANGCI_LINT) run --build-tags smoke

.PHONY: vulncheck
vulncheck: $(GOVULNCHECK) ## Check the code and its dependencies for known vulnerabilities.
	$(GOVULNCHECK) ./...

.PHONY: verify
verify: manifests generate fmt ## Fail if generated files, formatting or go.mod are out of date.
	go mod tidy
	git diff --exit-code

.PHONY: test
test: manifests generate fmt vet $(SETUP_ENVTEST) ## Run the tests against a local control plane (envtest).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

.PHONY: test-smoke
test-smoke: build-installer $(SETUP_ENVTEST) ## Run the built binary with the rendered install manifest and its RBAC against envtest.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test -tags smoke -count=1 ./test/smoke

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the manager binary into ./bin.
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(LOCALBIN)/manager ./cmd

.PHONY: run
run: manifests generate ## Run the operator locally against the cluster in your kubeconfig.
	go run ./cmd --metrics-bind-address=0

.PHONY: docker-build
docker-build: ## Build the operator image for the local platform.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t $(IMG) .

.PHONY: docker-buildx
docker-buildx: ## Build the operator image for all PLATFORMS and push it.
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --build-arg VERSION=$(VERSION) -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the operator image.
	$(CONTAINER_TOOL) push $(IMG)

.PHONY: build-installer
build-installer: manifests $(KUSTOMIZE) ## Render CRD + operator into a single dist/install.yaml.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

.PHONY: install
install: manifests $(KUSTOMIZE) ## Install the CRD into the cluster.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: $(KUSTOMIZE) ## Remove the CRD (and with it every WebApp) from the cluster.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests $(KUSTOMIZE) ## Deploy the operator into the cluster.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: $(KUSTOMIZE) ## Remove the operator from the cluster.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found -f -

##@ Tools

$(CONTROLLER_GEN):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(KUSTOMIZE):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

$(SETUP_ENVTEST):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

$(GOLANGCI_LINT):
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOVULNCHECK):
	GOBIN=$(LOCALBIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
