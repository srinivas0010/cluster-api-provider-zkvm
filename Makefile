# cluster-api-provider-zkvm Makefile
#
# Usage:
#   make generate        — regenerate zz_generated.deepcopy.go
#   make manifests       — regenerate CRD YAML files under config/crd/bases/
#   make build           — compile the controller binary locally
#   make docker-build    — build the container image
#   make docker-push     — push the container image to the registry
#   make deploy          — apply CRDs + controller into the current cluster (kubectl context)
#   make undeploy        — remove everything deployed by "make deploy"
#   make test            — run unit tests

# Image name and tag — override with: IMG=myregistry/myimage:tag make docker-build
IMG ?= cluster-api-provider-zkvm:dev

# controller-gen version to use for CRD/deepcopy generation.
CONTROLLER_GEN_VERSION ?= v0.14.0

# Directories.
LOCALBIN    ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

# Go settings.
GOARCH  ?= s390x
GOOS    ?= linux

.PHONY: all
all: generate manifests build

# ── Code generation ───────────────────────────────────────────────────────────

.PHONY: generate
generate: controller-gen
	## Regenerate zz_generated.deepcopy.go for all API types.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
manifests: controller-gen
	## Regenerate CRD YAML files and RBAC marker annotations.
	$(CONTROLLER_GEN) \
		crd \
		rbac:roleName=cluster-api-provider-zkvm-manager-role \
		webhook \
		paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

# ── Build ─────────────────────────────────────────────────────────────────────

.PHONY: build
build:
	## Build the controller binary for s390x.
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o bin/manager ./main.go

# ── Docker ────────────────────────────────────────────────────────────────────

.PHONY: docker-build
docker-build:
	## Build the controller container image.
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	## Push the controller container image to the registry.
	docker push $(IMG)

# ── Deploy / undeploy ─────────────────────────────────────────────────────────

.PHONY: deploy
deploy: manifests
	## Apply CRDs + RBAC + controller Deployment into the current kubectl context.
	cd config/manager && kustomize edit set image cluster-api-provider-zkvm=$(IMG)
	kubectl apply -k config/default

.PHONY: undeploy
undeploy:
	## Remove everything deployed by "make deploy".
	kubectl delete -k config/default --ignore-not-found

# ── Tests ─────────────────────────────────────────────────────────────────────

.PHONY: test
test:
	## Run unit tests.
	go test ./... -v

# ── Tools ─────────────────────────────────────────────────────────────────────

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	## Download controller-gen if not present.
	test -s $(CONTROLLER_GEN) || \
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)
