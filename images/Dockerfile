# runplane base image for GitHub Actions jobs. The build args select the
# Ubuntu release: UBUNTU is the tag of the ubuntu image, and IMAGE_OS is the
# ImageOS of the GitHub-hosted runners for that release. images/images.go
# names the builtin images and gives their build args.
#
# runplane starts runplane-init as the first process of each sandbox, so this
# image has no ENTRYPOINT. runplane-init starts dockerd and the runner as the
# user "runner". See images/README.md for custom images FROM this image.
ARG UBUNTU
FROM ubuntu:${UBUNTU}
ARG UBUNTU
ARG IMAGE_OS

LABEL org.opencontainers.image.title="runplane runner, Ubuntu ${UBUNTU}" \
      org.opencontainers.image.description="Base image for runplane sandboxes" \
      org.opencontainers.image.source="https://github.com/runplane/runplane" \
      dev.runplane.managed=true

# Versions for the tool cache: major versions of Node.js, minor versions of
# Python and Go. The build installs the latest release of each.
ARG NODE_VERSIONS="22 24 26"
ARG NODE_DEFAULT=24
ARG PYTHON_VERSIONS="3.11 3.12 3.13 3.14"
ARG GO_VERSIONS="1.27"

ENV LANG=C.UTF-8 \
    TZ=Etc/UTC \
    ImageOS=${IMAGE_OS} \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

# Base packages and the libraries that actions/runner needs
# (bin/installdependencies.sh). The first available name of each group wins.
RUN export DEBIAN_FRONTEND=noninteractive; \
    apt-get update -q; \
    first() { for p in "$@"; do if apt-cache show "$p" >/dev/null 2>&1; then echo "$p"; return; fi; done; echo "none of: $*" >&2; return 1; }; \
    apt-get install -y -q --no-install-recommends \
      ca-certificates curl wget gnupg git git-lfs jq zstd unzip zip xz-utils bzip2 \
      build-essential pkg-config cmake \
      python3 python3-pip python3-venv python3-dev \
      sudo locales tzdata lsb-release openssh-client rsync file less vim-tiny \
      iproute2 iptables iputils-ping dnsutils netcat-openbsd procps psmisc time \
      libkrb5-3 zlib1g \
      "$(first liblttng-ust1t64 liblttng-ust1 liblttng-ust0)" \
      "$(first libssl3t64 libssl3 libssl1.1)" \
      "$(first libicu80 libicu79 libicu78 libicu77 libicu76 libicu75 libicu74 libicu72 libicu70)"; \
    sed -i 's/^# *en_US.UTF-8/en_US.UTF-8/' /etc/locale.gen; \
    locale-gen; \
    rm -rf /var/lib/apt/lists/*

RUN export DEBIAN_FRONTEND=noninteractive; \
    arch=$(dpkg --print-architecture); \
    codename=$(. /etc/os-release && echo "$VERSION_CODENAME"); \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli.gpg; \
    echo "deb [arch=$arch signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $codename stable" \
      > /etc/apt/sources.list.d/docker.list; \
    echo "deb [arch=$arch signed-by=/etc/apt/keyrings/githubcli.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update -q; \
    apt-get install -y -q --no-install-recommends \
      docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin gh; \
    ln -s /usr/libexec/docker/cli-plugins/docker-compose /usr/local/bin/docker-compose; \
    rm -rf /var/lib/apt/lists/*

# The runner user, and the folders that runplane-init uses.
RUN useradd --create-home --uid 1001 --shell /bin/bash --groups docker runner; \
    echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner; \
    chmod 0440 /etc/sudoers.d/runner; \
    mkdir -p /run/runplane /mnt/scratch /home/runner/actions-runner "$RUNNER_TOOL_CACHE"; \
    chown runner:runner /home/runner/actions-runner; \
    printf '%s\n' "LANG=$LANG" "ImageOS=$ImageOS" "RUNNER_TOOL_CACHE=$RUNNER_TOOL_CACHE" \
      "AGENT_TOOLSDIRECTORY=$AGENT_TOOLSDIRECTORY" >> /etc/environment

# The tool cache has the layout of the GitHub-hosted runners, so that
# actions/setup-node, setup-python, and setup-go find these versions and do not
# download them: <tool>/<version>/<arch>/ and a <tool>/<version>/<arch>.complete file.
# The Python test suites and static libpython archives use 1 GB, and jobs
# seldom need them, so the build removes them.
RUN case $(dpkg --print-architecture) in \
      amd64) arch=x64 goarch=amd64 ;; \
      arm64) arch=arm64 goarch=arm64 ;; \
    esac; \
    cd /tmp; \
    curl -fsSL https://nodejs.org/dist/index.json -o node.json; \
    for major in $NODE_VERSIONS; do \
      v=$(jq -r --arg m "v$major." 'map(select(.version | startswith($m)))[0].version' node.json); \
      file=node-$v-linux-$arch.tar.xz; \
      curl -fsSL "https://nodejs.org/dist/$v/$file" -o "$file"; \
      curl -fsSL "https://nodejs.org/dist/$v/SHASUMS256.txt" | grep " $file\$" | sha256sum -c -; \
      dir=$RUNNER_TOOL_CACHE/node/${v#v}/$arch; \
      mkdir -p "$dir"; \
      tar -xJf "$file" -C "$dir" --strip-components=1 --no-same-owner; \
      touch "$dir.complete"; \
      rm "$file"; \
    done; \
    default=$(ls -d "$RUNNER_TOOL_CACHE/node/$NODE_DEFAULT".*/"$arch"); \
    ln -s "$default"/bin/* /usr/local/bin/; \
    \
    curl -fsSL https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json -o python.json; \
    os=$(. /etc/os-release && echo "$VERSION_ID"); \
    for minor in $PYTHON_VERSIONS; do \
      url=$(jq -r --arg m "$minor." --arg os "$os" --arg arch "$arch" \
        'first(.[] | select(.stable and (.version | startswith($m))) | .files[] | select(.platform == "linux" and .platform_version == $os and .arch == $arch)) | .download_url' python.json); \
      mkdir python; \
      curl -fsSL "$url" | tar -xz -C python; \
      (cd python && bash ./setup.sh >/dev/null); \
      rm -rf python; \
    done; \
    rm -rf "$RUNNER_TOOL_CACHE"/Python/*/"$arch"/lib/python3.*/test \
      "$RUNNER_TOOL_CACHE"/Python/*/"$arch"/lib/python3.*/config-*/libpython3.*.a; \
    \
    curl -fsSL 'https://go.dev/dl/?mode=json&include=all' -o go.json; \
    for minor in $GO_VERSIONS; do \
      read -r v sum < <(jq -r --arg m "go$minor" --arg arch "$goarch" \
        'first(.[] | select(.stable and (.version == $m or (.version | startswith($m + "."))))) | .files[] | select(.os == "linux" and .arch == $arch and .kind == "archive") | "\(.version) \(.sha256)"' go.json); \
      curl -fsSL "https://go.dev/dl/$v.linux-$goarch.tar.gz" -o go.tar.gz; \
      echo "$sum  go.tar.gz" | sha256sum -c -; \
      dir=$RUNNER_TOOL_CACHE/go/${v#go}/$arch; \
      mkdir -p "$dir"; \
      tar -xzf go.tar.gz -C "$dir" --strip-components=1 --no-same-owner; \
      touch "$dir.complete"; \
      rm go.tar.gz; \
    done; \
    \
    rm -rf /tmp/* /root/.cache; \
    chown -R runner:runner "$RUNNER_TOOL_CACHE"
