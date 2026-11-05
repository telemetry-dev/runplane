# runplane base images

runplane publishes two base images for the sandboxes. Each image is for `linux/amd64` and `linux/arm64`:

| Image | Base |
| --- | --- |
| `ghcr.io/runplane/runplane/runner-ubuntu-24.04:latest` | Ubuntu 24.04 (the default image of a pool) |
| `ghcr.io/runplane/runplane/runner-ubuntu-26.04:latest` | Ubuntu 26.04 |

One `Dockerfile` in this folder builds the two images. Its build args `UBUNTU` and `IMAGE_OS` select the Ubuntu release. `images.go` names the images and gives their build args. If runplane cannot pull a base image, it builds the image on the server from this `Dockerfile`.

## Contents

- git, git-lfs, curl, wget, jq, zstd, unzip, zip, xz, build-essential, cmake, pkg-config
- Docker Engine with buildx and compose (`docker compose` and `docker-compose`)
- Python 3 with pip, venv, and the headers (the Ubuntu packages)
- The GitHub CLI (`gh`)
- The libraries of the GitHub Actions runner (ICU, OpenSSL, Kerberos, LTTng)
- The tool cache in `/opt/hostedtoolcache` with Node.js 22, 24, and 26, Python 3.11 to 3.14, and Go 1.27
- Node.js 24 from the tool cache as `node`, `npm`, `npx`, and `corepack` in `/usr/local/bin`
- The user `runner` (UID 1001) with sudo access without a password, in the `docker` group
- The locales `C.UTF-8` and `en_US.UTF-8`

The tool cache has the layout of the GitHub-hosted runners. Thus `actions/setup-node`, `actions/setup-python`, and `actions/setup-go` use these versions and do not download them. `RUNNER_TOOL_CACHE` and `AGENT_TOOLSDIRECTORY` point to `/opt/hostedtoolcache`.

The images have no `ENTRYPOINT`. runplane starts `runplane-init` as the first process of the sandbox. `runplane-init` starts `dockerd` and the runner as the user `runner`.

## Make a custom image

To add tools to the sandboxes, make an image `FROM` a base image. For example:

```dockerfile
FROM ghcr.io/runplane/runplane/runner-ubuntu-24.04:latest

RUN apt-get update \
 && apt-get install -y --no-install-recommends postgresql-client openjdk-21-jdk-headless \
 && rm -rf /var/lib/apt/lists/*
```

To add a tool version to the tool cache, put it in `/opt/hostedtoolcache/<tool>/<version>/<arch>/`, and make the empty file `/opt/hostedtoolcache/<tool>/<version>/<arch>.complete`. The arch is `x64` or `arm64`. Then run `chown -R runner:runner /opt/hostedtoolcache`.

Obey these rules:

1. Do not set `USER`. `runplane-init` must start as root.
2. Do not remove the user `runner`, its sudo access, or its `docker` group.
3. Keep the folders `/home/runner/actions-runner`, `/run/runplane`, and `/mnt/scratch`.
4. Keep the image small. In VM mode, runplane copies the full image into a disk file on each server.
5. Remove the apt lists and the download files in the same `RUN` step.

Build the image for the architectures of your servers, and push it to a registry:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/<you>/runner-custom:latest --push .
```

runplane pulls the image without registry credentials, so make the image public.

Then add the image to runplane:

1. In the dashboard, open **Settings > Runners**.
2. Add the image with its reference, for example `ghcr.io/<you>/runner-custom:latest`.
3. On the **Runners** page, select the image for a pool.

runplane pulls the image on each server. When you push a new image with the same tag, select **Pull again** for the image. Then each new sandbox uses the new image.

## Build a base image

To build a base image on your computer, run this command from the repository root:

```sh
docker build -t runner-ubuntu-24.04 --build-arg UBUNTU=24.04 --build-arg IMAGE_OS=ubuntu24 images
```

The Dockerfile installs the latest release of each tool version in `NODE_VERSIONS`, `PYTHON_VERSIONS`, and `GO_VERSIONS`. To change the versions, use `--build-arg`, for example `--build-arg NODE_VERSIONS="24 26"`.

The `images.yml` workflow builds the two images each week and fails if an image uses more than 3 GB.
