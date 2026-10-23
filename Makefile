# Local builds. Outputs are git-ignored:
#   src/bin/   binary for the host platform
#   src/dist/  release archives (make dist)
#   dist/      packaged Helm chart
#
#   make build                 # binary
#   make dist                  # archives for every platform in PLATFORMS
#   make image                 # Docker image $(IMAGE):$(VERSION)
#   make chart                 # dist/serverscom-exporter-<version>.tgz
#   make all                   # build, image and chart
#   make demo                  # demo with the mock API, Prometheus and Grafana
#   make demo-down             # stop the demo and remove its data
#   make clean
#
# Release archives are src/dist/serverscom-exporter-<version>.<os>-<arch>.tar.gz
# with the binary and README.md in a directory of the same name, plus
# src/dist/sha256sums.txt. Same commit and version, same bytes. They need GNU
# tar and sha256sum; `make build` works anywhere.

SHELL       := bash
.SHELLFLAGS := -euo pipefail -c

# Version info, from git by default. VERSION has no leading v.
VERSION    ?= $(patsubst v%,%,$(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo dev))
REVISION   ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BRANCH     ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
# The commit time, so rebuilds are identical.
COMMIT_TIME := $(shell git log -1 --format=%ct 2>/dev/null || date +%s)
BUILD_DATE ?= $(shell date -u -d @$(COMMIT_TIME) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
# Evaluate the shell calls once.
VERSION    := $(VERSION)
REVISION   := $(REVISION)
BRANCH     := $(BRANCH)
BUILD_DATE := $(BUILD_DATE)

NAME      := serverscom-exporter
PKG       := ./cmd/serverscom-exporter
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

IMAGE ?= prometheus-serverscom-exporter

CHART_DIR := charts/serverscom-exporter
# Empty: version and appVersion from Chart.yaml.
CHART_VERSION ?=
APP_VERSION   ?=

# Must match the Dockerfile.
V       := github.com/prometheus/common/version
LDFLAGS := -s -w -X $(V).Version=$(VERSION) -X $(V).Revision=$(REVISION) \
           -X $(V).Branch=$(BRANCH) -X $(V).BuildDate=$(BUILD_DATE)

ARCHIVES := $(foreach p,$(PLATFORMS),src/dist/$(NAME)-$(VERSION).$(subst /,-,$(p)).tar.gz)

.PHONY: all build dist image chart demo demo-down clean FORCE

all: build image chart

build:
	cd src && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(NAME) $(PKG)

dist: $(ARCHIVES)
	cd src/dist && sha256sum -- $(notdir $(ARCHIVES)) > sha256sums.txt

# $* is <os>-<arch>. Always rebuilt: go build caches, and the version may not
# change between commits.
DIR  = $(NAME)-$(VERSION).$*
WORK = src/dist/.work-$*
src/dist/$(NAME)-$(VERSION).%.tar.gz: FORCE
	rm -rf $(WORK) && mkdir -p $(WORK)/$(DIR)
	cd src && CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(word 2,$(subst -, ,$*)) \
	  go build -trimpath -ldflags '$(LDFLAGS)' -o ../$(WORK)/$(DIR)/$(NAME) $(PKG)
	cp README.md $(WORK)/$(DIR)/
	tar -C $(WORK) --sort=name --owner=0 --group=0 --numeric-owner --mode=go-w \
	  --mtime=@$(COMMIT_TIME) -cf - $(DIR) | gzip -9n > $@
	rm -rf $(WORK)

image:
	docker build -t $(IMAGE):$(VERSION) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg REVISION=$(REVISION) \
	  --build-arg BRANCH=$(BRANCH) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  src/

chart:
	helm package $(CHART_DIR) --destination dist \
	  $(if $(CHART_VERSION),--version $(CHART_VERSION)) \
	  $(if $(APP_VERSION),--app-version $(APP_VERSION))

# Demo in Docker, see demo/docker-compose.yml. Grafana on
# http://localhost:3000/d/serverscom-exporter.
DEMO := docker compose -f demo/docker-compose.yml

demo:
	$(DEMO) up -d --build

demo-down:
	$(DEMO) down -v

clean:
	rm -rf src/bin src/dist dist
