# runplane build. "make help" shows the targets.

PKG     := github.com/runplane/runplane
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --verify -q HEAD)
REPO    ?= runplane/runplane
# Base64 of the raw 32-byte ed25519 key that verifies SHA256SUMS.sig (self-update).
UPDATE_PUBLIC_KEY ?=
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Repo=$(REPO) \
	-X $(PKG)/internal/update.PublicKey=$(UPDATE_PUBLIC_KEY)

SHA256SUM := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)
SHELLSCRIPTS := install.sh kernel/build.sh $(wildcard deploy/*.sh)

# The init binary has one path for all architectures, so builds must not overlap.
.NOTPARALLEL:
.PHONY: help web init build binary build-linux-amd64 build-linux-arm64 dist sums test lint openapi dev fake e2e runner-smoke clean

help:
	@echo 'make build              web, init, and bin/runplane for this OS and arch'
	@echo 'make build-linux-amd64  bin/runplane-linux-amd64'
	@echo 'make build-linux-arm64  bin/runplane-linux-arm64'
	@echo 'make dist               release archives, install.sh, and SHA256SUMS in dist/'
	@echo 'make web                frontend build into internal/webui/dist'
	@echo 'make init               runplane-init for GOARCH into internal/initbin/bin'
	@echo 'make test               go test, also of the dev seed (tag devseed)'
	@echo 'make lint               go vet (this OS and linux) and shellcheck'
	@echo 'make openapi            OpenAPI spec and TypeScript types for the frontend'
	@echo 'make dev                backend (dev mode, with the dev-seed command) and the Vite dev server'
	@echo 'make fake               backend (dev mode) against the fake GitHub and the fake runner'
	@echo 'make e2e                test of the whole job flow in dev mode (Docker and the base image)'
	@echo 'make runner-smoke       the real actions/runner in the local base image (Docker)'
	@echo 'make clean              remove build output'

web:
	pnpm -C web install
	pnpm -C web build:embed

# init gzips runplane-init, which makes the runplane binary about 4 MB
# smaller. internal/initbin unpacks it.
init:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "-s -w" \
		-o bin/runplane-init-linux-$(GOARCH) ./init
	gzip -9 -n -c bin/runplane-init-linux-$(GOARCH) > internal/initbin/bin/runplane-init

build: web
	$(MAKE) binary OUT=bin/runplane

# binary builds runplane for GOOS and GOARCH with the init binary of GOARCH.
binary: init
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(OUT) ./cmd/runplane

build-linux-amd64: web
	$(MAKE) binary GOOS=linux GOARCH=amd64 OUT=bin/runplane-linux-amd64

build-linux-arm64: web
	$(MAKE) binary GOOS=linux GOARCH=arm64 OUT=bin/runplane-linux-arm64

# size-check fails when a Linux binary has 30,000,000 bytes or more.
.PHONY: size-check
size-check:
	@status=0; for bin in bin/runplane-linux-amd64 bin/runplane-linux-arm64; do \
		size=$$(wc -c <$$bin) || exit 1; \
		size=$$((size)); \
		echo "$$bin: $$size bytes"; \
		[ $$size -lt 30000000 ] || { echo "$$bin is too large: the limit is 30,000,000 bytes" >&2; status=1; }; \
	done; exit $$status

# dist names the archives as internal/update expects: runplane_<version
# without v>_linux_<arch>.tar.gz, with the runplane binary at the top. The
# install.sh of the release gets the repository and the signing key of the
# build as its defaults.
dist: build-linux-amd64 build-linux-arm64 size-check
	rm -rf dist && mkdir -p dist/tmp
	for arch in amd64 arm64; do \
		cp bin/runplane-linux-$$arch dist/tmp/runplane && \
		COPYFILE_DISABLE=1 tar -C dist/tmp -czf dist/runplane_$(VERSION:v%=%)_linux_$$arch.tar.gz runplane || exit 1; \
	done
	rm -rf dist/tmp
	sed -e 's|^RUNPLANE_REPO=.*|RUNPLANE_REPO=$${RUNPLANE_REPO:-$(REPO)}|' \
		-e 's|^RUNPLANE_PUBLIC_KEY=.*|RUNPLANE_PUBLIC_KEY=$${RUNPLANE_PUBLIC_KEY:-$(UPDATE_PUBLIC_KEY)}|' \
		install.sh >dist/install.sh
	$(MAKE) sums

# sums writes SHA256SUMS for all files in dist/. The release adds the guest
# kernels to dist/ first.
sums:
	cd dist && rm -f SHA256SUMS SHA256SUMS.sig && $(SHA256SUM) * > SHA256SUMS

test:
	go test ./...
	go test -tags devseed ./internal/devseed

lint:
	go vet ./...
	GOOS=linux go vet ./...
	go vet -tags devseed ./internal/devseed ./cmd/runplane
	shellcheck -s sh $(SHELLSCRIPTS)

openapi:
	pnpm -C web gen:api

# dev uses the devdocker driver (privileged Docker containers). Use it on a
# development machine only. It builds with the tag devseed, which adds the
# dev-seed command. To put demo data in an empty .dev/data first, run:
#   RUNPLANE_DATA_DIR=.dev/data RUNPLANE_CONFIG_DIR=.dev/etc go run -tags devseed ./cmd/runplane dev-seed
dev: init
	mkdir -p .dev/data .dev/etc
	pnpm -C web install
	RUNPLANE_DATA_DIR=.dev/data RUNPLANE_CONFIG_DIR=.dev/etc RUNPLANE_MODE=dev RUNPLANE_DEV=1 \
		RUNPLANE_DEV_INSECURE=1 go run -tags devseed ./cmd/runplane serve & \
	pnpm -C web dev & \
	wait

# fake runs runplane in dev mode against the fake GitHub
# (internal/github/fakegh) with the fake runner bundle. Connect GitHub in the
# dashboard (the fake confirms each step at once), add a runner label, and
# queue a job: curl -d '{"repo":"acme/api","name":"test","labels":["<label>"]}' localhost:9100/_fake/jobs
# Sandboxes reach the fake at host.docker.internal.
FAKE_DIR ?= .dev/fake
FAKE_GH  ?= 127.0.0.1:9100
fake: init
	mkdir -p $(FAKE_DIR)/data $(FAKE_DIR)/etc
	go run ./internal/github/fakegh/cmd/fakegh -addr $(FAKE_GH) \
		-runner-url http://host.docker.internal:$(lastword $(subst :, ,$(FAKE_GH))) & \
	RUNPLANE_DATA_DIR=$(FAKE_DIR)/data RUNPLANE_CONFIG_DIR=$(FAKE_DIR)/etc RUNPLANE_MODE=dev RUNPLANE_DEV_INSECURE=1 \
		RUNPLANE_GITHUB_API_URL=http://$(FAKE_GH)/api/v3 RUNPLANE_GITHUB_URL=http://$(FAKE_GH) \
		RUNPLANE_DEV_RUNNER_DIR=$(CURDIR)/testdata/fakerunner go run ./cmd/runplane serve & \
	wait

# e2e runs e2e/dev_test.go: runplane in dev mode, sandboxes in Docker, the
# fake GitHub, setup, jobs, scaling, a restart, a debug shell, the mirror, and
# the teardown. It skips without Docker or the base image
# runplane/runner-ubuntu-24.04:local (docker build --provenance=false -t
# runplane/runner-ubuntu-24.04:local images/ubuntu-24.04; e2e/helpers_test.go
# tells why).
e2e:
	go test -tags e2e -count=1 -timeout 20m -v ./e2e/

# runner-smoke downloads the latest actions/runner for this arch and starts
# it in the local base image (see e2e above for the docker build): the .NET
# libraries must be in the image, and a bad JIT config must end the runner.
runner-smoke:
	go test -tags integration -count=1 -timeout 20m -v -run TestRealRunnerInBaseImage ./internal/image/

# clean keeps the other files in .dev, such as notes.
clean:
	rm -rf bin dist .dev/data .dev/etc $(FAKE_DIR) internal/initbin/bin/runplane-init
	find internal/webui/dist -mindepth 1 ! -name .keep -delete 2>/dev/null || true
