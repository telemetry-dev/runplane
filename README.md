# runplane

runplane operates GitHub Actions jobs on your own servers. You install one binary on a Linux server, and you connect GitHub in the dashboard. Then each job operates in a new sandbox on your server, and runplane erases the sandbox after the job.

- One command installs runplane, the sandbox runtime, and a systemd service.
- runplane gets jobs from GitHub through the runner scale set API. A public URL and webhooks are not necessary.
- Warm sandboxes wait for jobs, so a job starts in approximately 1 to 2 seconds.
- A local Actions cache and a Docker Hub mirror keep cache data and images on your disk.
- More servers can operate sandboxes for one dashboard.
- The page of a job compares the CPU and memory of its last runs with its label, and recommends a larger or smaller label.
- **Settings > Members** shows the activity: the changes that each user made, newest first.

## Requirements

| Item | Requirement |
| --- | --- |
| OS | Ubuntu 22.04 or 24.04. Debian 12 and 13 can operate, but we do not do tests on Debian. |
| CPU | x86_64 or aarch64 |
| Init system | systemd |
| Access | root (the install script uses `sudo`) |
| Network | HTTPS connections to GitHub. Port 8484 for the dashboard. Ports 80 and 443 for HTTPS with a domain. |
| Disk | 20 GB or more of free space. Each sandbox gets a scratch disk. |

## Install

On the server, run this command:

```sh
curl -fsSL https://github.com/runplane/runplane/releases/latest/download/install.sh | sudo sh
```

The script does these steps:

1. It does a check of the OS, the CPU architecture, and KVM.
2. It selects the sandbox mode (refer to [The two modes](#the-two-modes)).
3. It downloads runplane and makes sure that the signature of `SHA256SUMS` and the SHA-256 of each file are correct (refer to [Release](#release)).
4. In container mode, it installs Docker Engine (only if the server does not have it) and Sysbox.
5. In VM mode, it installs Firecracker v1.17.0, the jailer, and the guest kernel. It also gives the user `runplane-vm` a range of uids and gids, one for each VM. If the base image of the release repository is not public on GHCR, it installs Docker Engine, because runplane then builds the base image on the server. Docker builds images only, and the jobs operate in VMs.
6. It writes `/etc/runplane/runplane.env` and the systemd units, then it starts `runplane.service`.
7. It runs `runplane doctor` and shows the dashboard URL and a one-time setup code.

To see each step before the install, add `--dry-run`. The dry run changes nothing on the server.

```sh
curl -fsSL https://github.com/runplane/runplane/releases/latest/download/install.sh | sudo sh -s -- --dry-run
```

Flags of `install.sh`:

| Flag | Use |
| --- | --- |
| `--mode auto\|container\|vm` | Sandbox mode. The default is `auto`. |
| `--version <v>` | Release to install. The default is the latest release. |
| `--repo <owner/name>` | GitHub repository of the releases. The default is `runplane/runplane`. |
| `--listen <addr>` | Dashboard address. The default is `:8484`. |
| `--domain <name>` | Domain of the dashboard. runplane serves HTTPS at this domain (refer to [Security](#security)). |
| `--token`, `--primary`, `--fingerprint` | Installs an agent (refer to [Add servers](#add-servers)). |
| `--binary <path>`, `--kernel <path>` | Installs local files, not the release files (refer to [Install from source](#install-from-source)). |
| `--dry-run` | Shows each action and changes nothing. |
| `--uninstall` | Stops and removes runplane. The script asks before it removes your data. |
| `--yes` | Answers yes to all questions. |

In VM mode, the script installs Firecracker v1.17.0, which the runplane tests use. To install a different release, set `RUNPLANE_FIRECRACKER_VERSION` to its tag, for example `v1.16.1`. Use v1.13 or newer. Older releases do not add `pci=off` to the guest kernel command line, and then the guest kernel looks for PCI devices at each boot.

```sh
curl -fsSL https://github.com/runplane/runplane/releases/latest/download/install.sh | sudo RUNPLANE_FIRECRACKER_VERSION=v1.16.1 sh
```

To update runplane, run the install command again. You can also select the update in **Settings > General**, or run `sudo runplane update` on the server. In VM mode, the update also replaces the guest kernel with the kernel of the release. A restart of runplane does not stop the jobs that operate. A shutdown of the server stops the jobs that operate with the result `cancelled`.

The primary updates its agents. When an agent connects with a different version, the primary sends it the update to its own release. After an update of the primary, the agents connect again and get the new release. The primary tries each version one time on each agent. If the update is not successful, the **Servers** page shows **Update failed** after the name of the server. The page of the server shows the reason. You can start the update again with **Update agent** in the menu of the server. A primary that is not a release build does not update its agents.

You can also update an agent by hand. Run this command on the agent server:

```sh
curl -fsSL http://203.0.113.10:8484/install.sh | sudo sh
```

The primary serves this script. Thus the agent gets the version of the primary. On an agent, the script keeps the registration in `/etc/runplane/agent.json` and does not install a primary. On the agent server, `sudo runplane update` installs the latest release.

To remove runplane, run this command:

```sh
curl -fsSL https://github.com/runplane/runplane/releases/latest/download/install.sh | sudo sh -s -- --uninstall
```

The uninstall keeps Docker, Sysbox, and Firecracker installed.

### Install from source

If there is no release, you can install a build of the source code. The computer for the build must have Go, Node.js, and pnpm (refer to [Development](#development)).

1. Build the binary for the CPU of the server: `make build-linux-amd64` or `make build-linux-arm64`.
2. Copy `bin/runplane-linux-<arch>` and `install.sh` to the server.
3. On the server, run `sudo sh install.sh --binary ./runplane-linux-<arch>`.

In VM mode, the script must also have a guest kernel:

1. Build the kernel with `kernel/build.sh x86_64` or `kernel/build.sh aarch64`.
2. Copy `kernel/out/vmlinux-x86_64` or `kernel/out/vmlinux-arm64` to the server.
3. Add `--kernel ./vmlinux-<arch>` to the install command.

If you do not have a kernel, add `--mode container`.

If the primary is not a release build, the primary cannot give a release version to its agents. Thus, install each agent from the same build. Add the `--token`, `--primary`, and `--fingerprint` values from the **Add server** dialog:

```sh
sudo sh install.sh --binary ./runplane-linux-<arch> --token rp_srv_... --primary 203.0.113.10:8485 --fingerprint sha256:...
```

## First job

1. Open the dashboard URL that the install script shows, for example `http://203.0.113.10:8484`.
2. Type the setup code to make the admin account.
3. Select **Connect GitHub**.
4. In **Organization**, type the name from the URL of the organization, then select the organization in the list. For your personal account, select **Personal account**.
5. On GitHub, select **Create GitHub App**.
6. Install the app on your repositories.
7. In a workflow file, set `runs-on` to the label of the default pool:

   ```yaml
   jobs:
     test:
       runs-on: runplane
       steps:
         - uses: actions/checkout@v5
         - run: make test
   ```

8. Push the change.

runplane can also make the change for you. On the **First job** page or on the **GitHub** tab of **Settings**, select a repository, then select **Open pull request**. The pull request sets `runs-on` to the label for each job on `ubuntu-latest` or `ubuntu-24.04`. If the GitHub App is from an earlier release, add read and write access to Contents, Pull requests, and Workflows in the app settings on GitHub first.

The job shows on the **Jobs** page of the dashboard. If you lose the setup code, run `sudo runplane setup-code` on the server.

## The two modes

Each job gets a new sandbox. runplane has two types of sandbox:

| Item | VM mode (Firecracker) | Container mode (Sysbox) |
| --- | --- | --- |
| Host requirement | `/dev/kvm` | Docker Engine (not the snap package) and Sysbox 0.7.1 or newer |
| Kernel | One kernel for each job | The host kernel for all jobs |
| Root in a job | Root in the VM only | Root in a user namespace |
| Docker in jobs | Yes | Yes |
| Isolation between jobs | Strong | Weaker |
| Use for | Private repositories, and public repositories with care | Your private repositories only |

The install script selects the mode:

1. If runplane is on the server, the script keeps the mode of runplane. The primary keeps its mode in the dashboard settings. An agent keeps its mode in `/etc/runplane/runplane.env`.
2. If `/dev/kvm` exists, the script selects VM mode.
3. If the CPU has virtualization extensions but `/dev/kvm` does not exist, the script loads the `kvm` module and does the check again.
4. In all other cases, the script selects container mode.

Most VPS plans have no KVM, so container mode is the usual mode on a VPS. Dedicated servers and some cloud instances have KVM. To do the KVM check manually, use these commands:

```sh
ls -l /dev/kvm
grep -cE 'vmx|svm' /proc/cpuinfo
```

To select a mode, add `--mode container` or `--mode vm` to the install command. To change the mode of an installed server, run the install command again with `--mode`. On the primary, the script writes the new mode into the settings with `runplane set-mode`. On an agent server, use the install command that the primary serves (refer to [Install](#install)).

## Add servers

The first server is the primary. The primary has the dashboard, the database, and the GitHub credentials. Each other server operates the `runplane-agent` service. An agent connects to the primary on port 8485 with TLS, and it operates sandboxes for the primary. An agent has no database and no GitHub credentials.

To add a server:

1. In the dashboard, open **Servers**.
2. Select **Add server**.
3. Copy the install command, which contains a one-time token.
4. On the new server, run the command:

   ```sh
   curl -fsSL https://github.com/runplane/runplane/releases/download/v1.4.2/install.sh | sudo sh -s -- --repo runplane/runplane --version v1.4.2 --token rp_srv_... --primary 203.0.113.10:8485 --fingerprint sha256:...
   ```

The command gives the new server the address and the certificate fingerprint of the primary. The agent accepts only a primary with that fingerprint. With a domain, the command gets the script from the primary over HTTPS; without one, it gets the release script from GitHub, because a script that comes over plain HTTP could be changed on its way. You can also install an agent over SSH from the **Add server** dialog.

Make sure that the new server can connect to port 8485 of the primary. The agent opens the connection, thus a firewall on the agent server can block all incoming connections.

An agent must run the same release as the primary. The primary updates each agent with a different version when the agent connects (refer to [Install](#install)).

## Security

**Warning:** Use runplane only with private repositories. A pull request from a fork of a public repository can run code on your server.

In container mode, runplane always rejects jobs from public repositories. All Sysbox containers share the host kernel and one ID mapping, thus the isolation between jobs is weaker. In VM mode, runplane rejects jobs from public repositories and from fork pull requests, unless you change these settings. You can let jobs from public repositories run only if each server operates in VM mode.

| Item | Container mode (Sysbox CE) | VM mode (Firecracker) |
| --- | --- | --- |
| Kernel | The host kernel | One kernel for each job |
| Root of the job on the host | A user of one range for all containers | No host user (KVM) |
| Network model | One Docker bridge, with the MAC and IP binding in `bridge runplane` | A TAP with a /30 for each VM |
| AppArmor | The Sysbox profile | Not applicable |
| Host daemon on the `/proc` path | `sysbox-fs` (Sysbox issue 1018) | None |
| Public repositories | Rejected | Permitted with the setting |

Sandboxes cannot connect to the host, to other sandboxes, to private networks, to `169.254.169.254`, or to the Azure host endpoint `168.63.129.16`. They can connect only to the public internet and to the runplane cache service on the host.

The cache service (port 8486) accepts connections only from the sandboxes of the server and from the server itself. A firewall rule of runplane drops the connections from other computers. Ports 8484 (dashboard), 8485 (agents), and 2222 (debug shells) stay open.

runplane encrypts the secrets in its database with `/etc/runplane/secret.key`: the GitHub App key, the S3 secret key, and the notification URLs. The backups of runplane contain the database only, not the key. Keep a copy of `secret.key` in a safe place that is not on the server. Refer to [Restore from a backup](#restore-from-a-backup).

The dashboard uses HTTP on port 8484. To get HTTPS with Let's Encrypt, set a domain in **Settings > General**, or add `--domain ci.example.com` to the install command. The A or AAAA record of the domain must point to the server, with no proxy. **Settings > General** shows the IP address for the record, and it shows if the record is correct. Ports 80 and 443 must be free on the server and open in the firewall. If a different web server uses these ports, the certificate row of the settings shows the error. If you do not set a domain, open the dashboard through an SSH tunnel:

```sh
ssh -L 8484:localhost:8484 user@server
```

To set the domain from the shell of the primary, run `sudo runplane set-domain ci.example.com`. Then run `sudo systemctl restart runplane`. An empty domain (`''`) stops HTTPS.

The primary also gives SSH access to the debug shell of a sandbox on port 2222 (`RUNPLANE_SSH_LISTEN` in `runplane.env`). The job actor connects with a key from GitHub. To give administrators access to all debug sandboxes, put their public keys in `/etc/runplane/authorized_keys`.

If you do not know the admin password, run `sudo runplane reset-password <email>` on the primary. The command shows a new password and stops all sessions of that user.

If 10 logins for an email are not successful in 15 minutes, runplane rejects new logins for that email. A browser that logged in to that account before has a different count, thus it can still log in. To stop the lock for all browsers, run `sudo systemctl restart runplane`.

## Files

| Path | Contents |
| --- | --- |
| `/usr/local/bin/runplane` | The runplane binary |
| `/etc/runplane/runplane.env` | Mode and ports of the service |
| `/etc/runplane/secret.key` | Encryption key for the secrets in the database |
| `/etc/runplane/agent.json` | Agent only: primary address, fingerprint, server ID, and secret |
| `/var/lib/runplane/` | Database, job logs, caches, images, and sandboxes |
| `/etc/systemd/system/runplane.service` | Primary service (`runplane-agent.service` on an agent) |
| `/etc/systemd/system/runplane.slice` | Low CPU and disk weight for the sandboxes |
| `/etc/subuid`, `/etc/subgid` | VM mode: the uids and gids of the VMs, `runplane-vm:1879048192:16384`. The Firecracker process of each VM has its own uid and gid. |
| `/etc/modules-load.d/runplane.conf` | VM mode: the kernel module `tun`, which loads at each boot |

To examine the service, use `journalctl -u runplane -f` and `sudo runplane doctor`.

## Restore from a backup

At 03:00 UTC each day, runplane writes a backup of its database to `/var/lib/runplane/backups/`, and to S3 if you set a bucket. A backup contains no copy of `/etc/runplane/secret.key`. Without that key, runplane cannot read the secrets in the backup. Thus, keep these copies in a safe place that is not on the server:

- `/etc/runplane/secret.key`, which encrypts the secrets in the database
- `/var/lib/runplane/tls/`, which has the certificate that the agents accept.

To restore a backup on a new server:

1. Install runplane.
2. Stop runplane: `sudo systemctl stop runplane`
3. Put your copy of `secret.key` in `/etc/runplane/secret.key` with mode 0600.
4. If you have a copy of the `tls` folder, put it in `/var/lib/runplane/tls/`.
5. Decompress the backup: `sudo sh -c 'gunzip -c runplane-<time>.db.gz > /var/lib/runplane/runplane.db'`
6. Set the mode of the database file: `sudo chmod 0600 /var/lib/runplane/runplane.db`
7. Remove `/var/lib/runplane/runplane.db-wal` and `/var/lib/runplane/runplane.db-shm` if they are on the server.
8. Start runplane: `sudo systemctl start runplane`

The backup keeps the sandbox mode in its settings. To use a different mode, run `sudo runplane set-mode container` or `sudo runplane set-mode vm` before step 8.

The agents connect to the address of the old server. If the new server has a different address, install each agent again.

If you do not have the old `secret.key`, the jobs, pools, users, and settings stay, but runplane cannot read the secrets. Do these steps:

1. On the **GitHub** page, disconnect GitHub and connect it again.
2. In **Settings**, type the S3 secret key and the URLs of the notification channels again.
3. If you do not have the old `tls` folder, install each agent again. The agents do not accept the new certificate.

## Base images

Pools use `ghcr.io/runplane/runplane/runner-ubuntu-24.04` by default. The image contains Docker, Node.js, Python, Go, and the GitHub CLI. It also has a tool cache for `actions/setup-node`, `setup-python`, and `setup-go`. To add tools, make a custom image `FROM` the base image. Refer to [images/README.md](images/README.md).

If a server cannot pull the image from GHCR, runplane builds it on the server from the Dockerfile in its binary, with Docker. The first build takes some minutes. The check "Base image source" of `runplane doctor` shows which of the two the server does.

## Development

You must have Go (the version in `go.mod`), Node.js 24 or newer, pnpm, and Docker.

**Warning:** Use `make dev` only on a development computer. The `devdocker` driver starts privileged Docker containers, which give jobs root access to the computer.

```sh
make dev
```

`make dev` starts the backend with `RUNPLANE_MODE=dev` and its data in `.dev/`. At the same time, it starts the Vite dev server for the frontend.

Other commands:

| Command | Result |
| --- | --- |
| `make build` | Frontend, `runplane-init`, and `bin/runplane` for this computer |
| `make build-linux-amd64`, `make build-linux-arm64` | `bin/runplane-linux-<arch>` |
| `make test` | `go test ./...` |
| `make runner-smoke` | The latest `actions/runner` of this arch in the image `runplane/runner-ubuntu-24.04:local`: `Runner.Listener --version` must start, and `run.sh` with a bad JIT config must stop. It needs Docker and the network. |
| `make lint` | `go vet` for this OS and for Linux, and `shellcheck` |
| `make openapi` | The OpenAPI spec and the TypeScript types in `web/src/api/` |
| `make dist` | Release archives, `install.sh`, and `SHA256SUMS` in `dist/`. It runs `make size-check` first. |
| `make size-check` | Stops with an error if `bin/runplane-linux-amd64` or `bin/runplane-linux-arm64` has 30,000,000 bytes or more |
| `sh deploy/test-install.sh` | Dry runs of `install.sh` in Ubuntu, Debian, and Fedora containers |
| `kernel/build.sh --config-only` | A check of the guest kernel config, without a kernel build |

The tests of netlink, nftables, loop devices, and mounts must have root access on Linux. To do these tests on macOS, use a Linux container:

```sh
docker run --rm --privileged -v "$PWD":/src -v "$HOME/go/pkg/mod":/go/pkg/mod -w /src golang:1.27 go test ./internal/...
```

## Release

1. Make an ed25519 key one time: `openssl genpkey -algorithm ed25519 -out runplane-signing.pem`.
2. Keep a copy of the key in a safe location.
3. Add the contents of the key file as the repository secret `RUNPLANE_SIGNING_KEY`.
4. Push a tag: `git tag v1.2.0 && git push origin v1.2.0`.

The `release.yml` workflow then does these steps:

1. It builds the frontend, `runplane-init`, and `runplane` for `linux/amd64` and `linux/arm64`.
2. It builds the guest kernels `vmlinux-x86_64` and `vmlinux-arm64` with `kernel.yml`.
3. It writes `SHA256SUMS` and signs it with ed25519 (`SHA256SUMS.sig`).
4. It makes the GitHub release with the archives, the kernels, `install.sh`, and the two checksum files.

The binaries contain the public key. Before a self-update, runplane makes sure that the signature of `SHA256SUMS` is correct. The `install.sh` of the release and the script that a primary serves also contain the key. Before an install, these scripts make sure that the signature is correct.

A person who can change the release files can also change the `install.sh` of the release. Thus the signature protects self-updates and agent installs most: their key comes from a binary that you installed before. To protect new installs of a primary, set release immutability in the GitHub settings of the repository. Then nobody can change the files of a published release.

**Caution:** Do not change the key after the first release. A binary with the earlier key cannot do a self-update to a release with a new key. On these servers, run the install command of the new release again.

The `images.yml` workflow pushes the base images to GHCR each week and after each change in `images/`. The builds use the `runplane` label. QEMU builds the architecture that the runner does not have. After the first push, set the visibility of each image package on GHCR to public. Then servers pull the base images without a login, and VM mode does not need Docker. To do a check, run `docker logout ghcr.io && docker pull ghcr.io/<owner>/<repo>/runner-ubuntu-24.04:latest`.
