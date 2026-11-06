# The image of the sandbox tests that start real containers (TestContainerMode
# in sysbox and TestSandbox in devdocker): bash, curl, Docker, the runner user
# of the builtin images, and their label. It is much smaller than a builtin
# image. CI builds it with:
#   docker build -t runplane-test internal/sandbox/sysbox/sysboxtest
FROM ubuntu:24.04
RUN apt-get update -q \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends ca-certificates curl docker.io \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1001 --groups docker runner
LABEL dev.runplane.managed=true
