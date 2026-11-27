# NetMeta developer targets. The stack itself runs with `docker compose`.

TAG      ?= latest
REGISTRY ?= ghcr.io/slothnetworks
CMDS     := geoip migrate

.PHONY: all vet test lint vuln build images up down e2e

all: vet test build

vet:
	go vet ./...

test:
	go test -race ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping (https://golangci-lint.run/welcome/install/)"; \
	fi

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

build:
	go build ./...

images:
	@for cmd in $(CMDS); do \
		echo "building $(REGISTRY)/netmeta-$$cmd:$(TAG)"; \
		docker build --build-arg CMD=$$cmd -t $(REGISTRY)/netmeta-$$cmd:$(TAG) . || exit 1; \
	done

up:
	docker compose up -d --build

down:
	docker compose down

e2e:
	./test/e2e.sh
