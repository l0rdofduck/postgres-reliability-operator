IMAGE ?= pgc/postgres-patroni:16
OPERATOR_IMAGE ?= pgc/operator:dev
KIND_CLUSTER ?= pgc
NAMESPACE ?= pgc-demo
CLUSTER ?= demo

.PHONY: help fmt vet test build docker-build image operator-image load kind-up kind-down install deploy sample undeploy failover chaos logs demo

help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-16s %s\n", $$1, $$2}'

fmt: ## Format Go sources
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet: ## Run go vet
	go vet ./...

test: ## Run unit tests with the race detector
	go test -race ./...

build: ## Build the operator binary
	CGO_ENABLED=0 go build -o bin/operator ./cmd

docker-build: ## Build the operator container image
	docker build -t $(OPERATOR_IMAGE) .

image: ## Build the PostgreSQL + Patroni image
	docker build -t $(IMAGE) images/postgres-patroni

operator-image: docker-build

load: image docker-build ## Load both images into kind
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)
	kind load docker-image $(OPERATOR_IMAGE) --name $(KIND_CLUSTER)

kind-up: ## Create the kind cluster
	kind create cluster --config kind/cluster.yaml --name $(KIND_CLUSTER)

kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

install: ## Install CRDs
	kubectl apply -f config/crd/

deploy: install ## Deploy the operator
	kubectl apply -f config/rbac/
	kubectl apply -f config/manager/
	kubectl -n pgc-system rollout status deployment/pgc-operator --timeout=120s

sample: ## Create the demo PostgreSQL cluster
	kubectl apply -f config/samples/

undeploy: ## Remove the demo cluster and the operator
	kubectl delete -f config/samples/ --ignore-not-found
	kubectl delete -f config/manager/ --ignore-not-found
	kubectl delete -f config/rbac/ --ignore-not-found
	kubectl delete -f config/crd/ --ignore-not-found

failover: ## Switch the primary over and measure the leadership change
	./chaos/failover.sh $(NAMESPACE) $(CLUSTER)

chaos: failover ## Alias for failover

logs: ## Tail operator logs
	kubectl -n pgc-system logs deployment/pgc-operator -f

demo: kind-up image docker-build load deploy sample ## Full local demo
	@echo "Waiting for the cluster to become Ready..."
	kubectl -n $(NAMESPACE) wait --for=condition=Ready pgc/$(CLUSTER) --timeout=300s
	kubectl -n $(NAMESPACE) get pgc $(CLUSTER)
