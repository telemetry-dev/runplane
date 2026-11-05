# runplane build. "make help" shows the targets.

PKG     := github.com/runplane/runplane
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --verify -q HEAD)
REPO    ?= runplane/runplane
# Base64 of the raw 32-byte ed25519 key that verifies SHA256SUMS.sig (self-update).
UPDATE_PUBLIC_KEY ?=
# Client ID of the OAuth app of runplane, with the device flow on. The setup
# signs in to GitHub with it to list the accounts for the GitHub App. It is
# not a secret. Without it, the setup asks for the organization name.
GITHUB_CLIENT_ID ?=
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Repo=$(REPO) \
	-X $(PKG)/internal/update.PublicKey=$(UPDATE_PUBLIC_KEY) \
	-X $(PKG)/internal/github.ClientID=$(GITHUB_CLIENT_ID)

SHA256SUM := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)
SHELLSCRIPTS := install.sh kernel/build.sh $(wildcard deploy/*.sh)

# The init binary has one path for all architectures, so builds must not overlap.
.NOTPARALLEL:
.PHONY: help web init build binary build-linux-amd64 build-linux-arm64 size-check dist sums test lint openapi dev runner-smoke clean

help:
	@echo 'make build              web, init, and bin/runplane for this OS and arch'
	@echo 'make build-linux-amd64  bin/runplane-linux-amd64'
	@echo 'make build-linux-arm64  bin/runplane-linux-arm64'
	@echo 'make dist               release archives, install.sh, and SHA256SUMS in dist/'
	@echo 'make size-check         stop if bin/runplane-linux-<arch> has 30,000,000 bytes or more'
	@echo 'make web                frontend build into internal/webui/dist'
	@echo 'make init               runplane-init for GOARCH into internal/initbin/bin'
	@echo 'make test               go test'
	@echo 'make lint               go vet (this OS and linux) and shellcheck'
	@echo 'make openapi            OpenAPI spec and TypeScript types for the frontend'
	@echo 'make dev                backend (dev mode) and the Vite dev server'
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

lint:
	go vet ./...
	GOOS=linux go vet ./...
	shellcheck -s sh $(SHELLSCRIPTS)

openapi:
	pnpm -C web gen:api

# dev uses the devdocker driver (privileged Docker containers). Use it on a
# development machine only.
dev: init
	mkdir -p .dev/data .dev/etc
	pnpm -C web install
	RUNPLANE_DATA_DIR=.dev/data RUNPLANE_CONFIG_DIR=.dev/etc RUNPLANE_MODE=dev RUNPLANE_DEV=1 \
		RUNPLANE_DEV_INSECURE=1 go run -ldflags "-X $(PKG)/internal/github.ClientID=$(GITHUB_CLIENT_ID)" ./cmd/runplane serve & \
	pnpm -C web dev & \
	wait

# runner-smoke downloads the latest actions/runner for this arch and starts
# it in the local base image runplane/runner-ubuntu-24.04:local (docker build
# --provenance=false -t runplane/runner-ubuntu-24.04:local images/ubuntu-24.04):
# the .NET libraries must be in the image, and a bad JIT config must end the
# runner.
runner-smoke:
	go test -tags integration -count=1 -timeout 20m -v -run TestRealRunnerInBaseImage ./internal/image/

# clean keeps the other files in .dev, such as notes.
clean:
	rm -rf bin dist .dev/data .dev/etc internal/initbin/bin/runplane-init
	find internal/webui/dist -mindepth 1 ! -name .keep -delete 2>/dev/null || true
