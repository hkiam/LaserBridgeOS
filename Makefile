SHELL := /bin/sh

VERSION ?= 0.1.0
SOURCE_DATE_EPOCH ?= 1786579200
BUILDER_IMAGE := laserbridgeos-builder:$(VERSION)
ROOT_DIR := $(CURDIR)
HOST_UID := $(shell id -u)
HOST_GID := $(shell id -g)

.PHONY: all image test check fmt shellcheck run clean

all: test

image:
	mkdir -p dist
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) --build-arg SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) -f build/Dockerfile -t $(BUILDER_IMAGE) .
	docker run --rm --platform linux/amd64 -e VERSION=$(VERSION) -e SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) -e OUTPUT_UID=$(HOST_UID) -e OUTPUT_GID=$(HOST_GID) -v $(ROOT_DIR)/dist:/out $(BUILDER_IMAGE)

test:
	docker run --rm --user $(HOST_UID):$(HOST_GID) -e GOCACHE=/tmp/go-cache -v $(ROOT_DIR):/src -w /src/backend golang:1.23-alpine go test ./...

check:
	docker run --rm --user $(HOST_UID):$(HOST_GID) -e GOCACHE=/tmp/go-cache -v $(ROOT_DIR):/src -w /src/backend golang:1.23-alpine go vet ./...
	sh scripts/shell-files.sh | xargs -n1 sh -n
	sh scripts/check-boot-policy.sh
	sh scripts/check-web-ui.sh

fmt:
	docker run --rm --user $(HOST_UID):$(HOST_GID) -e GOCACHE=/tmp/go-cache -v $(ROOT_DIR):/src -w /src/backend golang:1.23-alpine gofmt -w .

shellcheck:
	docker run --rm -v $(ROOT_DIR):/mnt koalaman/shellcheck:stable $$(sh scripts/shell-files.sh)

run:
	mkdir -p .local-data .local-run
	docker run --rm --user $(HOST_UID):$(HOST_GID) -e GOCACHE=/tmp/go-cache -e LASERBRIDGE_CONFIG=/src/.local-data/config.yaml -e LASERBRIDGE_RUNTIME=/src/.local-run -e LASERBRIDGE_VERSION_FILE=/src/rootfs/etc/laserbridge/version -v $(ROOT_DIR):/src -w /src/backend -p 8088:8088 golang:1.23-alpine go run ./cmd/laserbridge serve --listen :8088 --web-root /src/web

clean:
	rm -rf dist .local-data .local-run
