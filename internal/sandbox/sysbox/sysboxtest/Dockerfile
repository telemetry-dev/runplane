# The image of the sandbox tests that start real containers (TestContainerMode
# in sysbox and TestSandbox in devdocker): bash, curl, Docker, the runner user
# of the builtin images, and their label. It is much smaller than a builtin
# image. CI builds it with:
#   docker build -t runplane-test internal/sandbox/sysbox/sysboxtest
#
# Docker in the sandbox gets a bridge outside 172.17.0.0/16. On a Linux host,
# dev sandboxes reach runplane at host.docker.internal, the docker0 address of
# the host in that range, and a docker0 in the sandbox would hide it.
FROM ubuntu:24.04
RUN apt-get update -q \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends ca-certificates curl docker.io \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1001 --groups docker runner \
    && mkdir -p /etc/docker \
    && echo '{"bip": "192.168.240.1/24"}' >/etc/docker/daemon.json
LABEL dev.runplane.managed=true
