# runplane backend plan

Draft, 4 November 2026.

runplane is a self-hosted platform for GitHub Actions runners. You install runplane on your server and connect GitHub. Then your jobs operate on that server in temporary sandboxes. The model is Coolify: one install command, one dashboard, and no paid runner service.

The frontend design is in `GUI.md` and `gui/`. This document is the plan for the backend. It uses these names:

| Name | What it is |
| --- | --- |
| Host | The server that operates runplane |
| Sandbox | The isolated place where one job operates |
| VM mode | Each sandbox is a Firecracker microVM (VM). The host must have KVM. |
| Container mode | Each sandbox is a Sysbox container. KVM is not necessary. |

## Decisions

| Topic | Decision |
| --- | --- |
| Isolation | Each job gets a new sandbox. runplane erases the sandbox after the job. |
| Mode | VM mode on hosts with KVM. Container mode on all other hosts. The install script selects the mode. |
| Host | A Linux VPS or a dedicated server. Ubuntu 24.04 is the primary target. |
| Language | Go. One static binary contains the API, the scheduler, and the two sandbox drivers. |
| Storage | SQLite. Postgres, Redis, and Kubernetes are not necessary. Container mode uses Docker Engine on the host. |
| GitHub connection | runplane makes a GitHub App with the manifest flow. runplane gets jobs through the runner scale set API. It polls GitHub, so a public URL and webhooks are not necessary. |
| Speed | Warm sandboxes wait for jobs. Local caches keep Docker images and Actions caches on the host disk. |
| Frontend | The frontend build is static files in the binary. The API is JSON with Server-Sent Events (SSE). |

## The two modes

The install script does a KVM check on the host. If `/dev/kvm` is available, runplane uses VM mode. If not, runplane uses container mode. You can change the mode in the configuration. For example, use container mode on a host with slow nested virtualization.

| Item | VM mode (Firecracker) | Container mode (Sysbox) |
| --- | --- | --- |
| Host requirement | `/dev/kvm` | Ubuntu 22.04 or 24.04, and Docker Engine (not the snap package) |
| Kernel | One kernel for each job | The host kernel for all jobs |
| Root in a job | Root in the VM only | Root in a user namespace. On the host, it is an unprivileged user. |
| Isolation between jobs | Strong | Weaker. Sysbox CE gives all containers the same ID mapping. |
| Docker in jobs | Yes, one daemon in each VM | Yes, one daemon in each container |
| Sandbox start | 125 ms or less to init (Firecracker specification) | Approximately 1 s (estimate) |
| Memory for each sandbox | Guest kernel plus the job | Only the job |
| Safe for | Private repositories. Public repositories only with care. | Your private repositories only |
| Known problems | Nested virtualization is slower | Sysbox issue #1018 can stop containers. Sysbox support is best effort. |

Most VPS plans have no KVM, so container mode is the usual mode on a VPS. Use VM mode for code that you do not trust.

### KVM by provider

| Provider | `/dev/kvm` in a VM | Mode |
| --- | --- | --- |
| Hetzner Cloud | No | Container |
| Linode | No | Container |
| Netcup VPS and root servers | No | Container |
| Contabo Cloud VPS | No | Container |
| Contabo VDS | Yes | VM |
| Hetzner dedicated servers (AX line, server auction) | Yes (bare metal) | VM |
| DigitalOcean Droplets (Intel) | Yes, but not formally documented | VM |
| AWS EC2 | Yes on C8i, M8i, R8i, and some 7th-generation Intel types (opt-in) | VM |
| Google Cloud | Yes on most Intel series (opt-in) | VM |
| Azure | Yes on many sizes, for example Dsv5 and Dadsv6. Not on B-series. | VM |
| Vultr | Only on VX1 | VM on VX1 |
| OVHcloud Public Cloud | Yes, but a live migration can cause a kernel panic in the nested VM | VM |

The table shows the status on 4 November 2026. Nested virtualization makes VMs slower than bare metal. In one Oracle test, random disk I/O in a nested Firecracker VM was approximately 1.8 times slower, and CPU speed was approximately 3% lower.

To do the KVM check manually, use these commands:

```sh
ls -l /dev/kvm
grep -cE 'vmx|svm' /proc/cpuinfo
```

After the install, `runplane doctor` does these checks again: KVM, kernel version, cgroup v2, nftables, IP forwarding, free disk space, Docker and Sysbox versions, Docker firewall rules, and sandbox internet access.

runplane supports Ubuntu 22.04 and 24.04 for container mode. Sysbox does not list Debian 12 and 13. Some users operate Sysbox on Debian 13, but with open issues.

## Isolation options

| Option | Isolation | Docker in jobs | Start time | KVM | Decision |
| --- | --- | --- | --- | --- | --- |
| Bare runner on the host | None | Host Docker, which gives root on the host | 0 s (always on) | No | Do not use |
| Docker container for each job | Shared kernel | Only with `--privileged` or the host Docker socket, which give root on the host | Approximately 1 s | No | Do not use |
| Container with Sysbox | Shared kernel and user namespaces | Yes | Approximately 1 s | No | **Use** in container mode |
| Incus system container | Shared kernel and user namespaces, with one ID mapping for each container | Yes, with `security.nesting=true` | Approximately 1 s (estimate) | No | Alternative if Sysbox development stops |
| Container with gVisor | Kernel in user space | Docker in gVisor cannot publish ports, so `services:` with `ports:` does not operate | Approximately 1 s | No | Do not use |
| Firecracker | Hardware (KVM) and a dedicated kernel | Yes, one Docker daemon in each VM | 125 ms or less to init, then the runner start | Yes | **Use** in VM mode |
| Cloud Hypervisor | Hardware (KVM) and a dedicated kernel | Yes | Less than 100 ms to user space (value from the project) | Yes, and host kernel 6.8 or newer | Possible driver for GPU jobs |
| Kata Containers | Hardware (KVM) | Yes | Containerd and a shim add time | Yes | Too many parts for one small host |

### Why not a bare runner

A bare runner gives each job full access to the host. A job can read the GitHub App private key of runplane. With this key, an attacker can make access tokens for all repositories of the installation. Files, processes, and Docker images also stay after each job. Thus one job can change the result of the next job.

### Why Firecracker on hosts with KVM

Each job gets a dedicated kernel, disk, network, and Docker daemon. The Firecracker specification gives 125 ms or less from start to `/sbin/init`, and 5 MiB or less of memory overhead for each VM. Actuated and Blacksmith use Firecracker for GitHub Actions jobs. The Firecracker project does tests on host kernels 5.10, 6.1, and 6.18. Cloud Hypervisor must have host kernel 6.8 or newer, so Firecracker operates on more hosts.

### Why Sysbox on other hosts

Sysbox starts each container in a user namespace. Root in the container is an unprivileged user on the host, and Docker operates in the container without `--privileged`. Coolify uses Sysbox for the same work. Sysbox has these limits:

- All containers share the host kernel. A kernel bug can let a job get access to the host.
- Sysbox CE gives all containers the same ID mapping. If a job gets access outside its container, it can also get access to the other containers.
- Support is best effort. One primary maintainer does most of the work, and the latest release is 0.7.1 (31 July 2026).
- Issue #1018 is open. Under heavy container activity, the `sysbox-fs` process can stop, and all containers stop with it.

Thus runplane uses container mode only for trusted code, and recommends VM mode where KVM is available.

## User flow

1. Install runplane: `curl -fsSL https://<your-domain>/install.sh | sudo sh`.
2. Open `http://<host-ip>:8484`.
3. Make the admin account with the one-time setup code that the install script shows.
4. Select **Connect GitHub**.
5. Select an organization or your personal account.
6. On GitHub, select **Create GitHub App**.
7. Install the app on your repositories.
8. In a workflow, set `runs-on` to the label of the default pool.
9. Push the change.

```yaml
jobs:
  test:
    runs-on: runplane
```

The install script does the KVM check. In VM mode, it installs Firecracker, the jailer, the guest kernel, and the base image. In container mode, it installs Docker Engine if necessary, then Sysbox, and pulls the base image. Then it starts the `runplane` systemd service.

## Architecture

```text
GitHub scale set API                     GitHub runner service
        ▲                                         ▲
        │ long poll, JIT configs                  │ the runner gets a job
        │                                         │
runplane (one Go binary) ── starts ──►  sandbox: runplane-init, dockerd, actions runner
  api · github · scheduler · sandbox · cache · store (SQLite)
```

| Package | Work |
| --- | --- |
| `api` | HTTP API, login sessions, SSE, embedded frontend |
| `github` | Manifest flow, installations, runner groups, scale sets, listeners, runner versions |
| `scheduler` | Pools, warm sandboxes, runner counts, host capacity |
| `sandbox` | The `Driver` interface, network rules, scratch disks |
| `sandbox/firecracker` | VM mode: Firecracker, jailer, TAP devices, vsock |
| `sandbox/sysbox` | Container mode: Docker Engine API, `sysbox-runc`, the #1018 watchdog |
| `image` | OCI images, ext4 root disks for VM mode, runner bundles |
| `cache` | Actions cache server, registry mirror, removal of unused entries |
| `store` | SQLite schema, migrations, queries |
| `init` | `runplane-init`, the first process in each sandbox |

Each mode is one `Driver`. The scheduler, the GitHub code, and the API do not know which mode operates. In Phase 5, the same interface operates over the network, so one runplane can control more hosts.

```go
package sandbox

type Driver interface {
	Start(ctx context.Context, spec Spec) error
	Stop(ctx context.Context, id string) error
	List(ctx context.Context) ([]Sandbox, error)
	Capacity(ctx context.Context) (Capacity, error)
	Events() <-chan Event
}
```

We use Go because of these facts:

- The official runner scale set client, `github.com/actions/scaleset`, is a Go library.
- Go makes one static binary with low memory use.
- Go has good Linux libraries for netlink, nftables, vsock, Docker, and SQLite without cgo.

| Item | Library |
| --- | --- |
| Scale sets and JIT configs | `github.com/actions/scaleset` |
| Docker Engine API (container mode) | `github.com/moby/moby/client` |
| TAP devices and routes | `github.com/vishvananda/netlink` |
| Firewall | `github.com/google/nftables` |
| SQLite without cgo | `modernc.org/sqlite` |
| OCI image pulls | `github.com/google/go-containerregistry` |
| Tar to ext4 (VM mode) | `github.com/Microsoft/hcsshim/ext4/tar2ext4` |
| vsock in the guest | `github.com/mdlayher/vsock` |
| Firecracker API | `net/http` on the API Unix socket. The Go SDK is not necessary. |

## GitHub connection

### The GitHub App

runplane makes the app with the GitHub App manifest flow, the same as Coolify. The browser does all redirects, so GitHub does not connect to runplane. The app has no webhook.

| Account type | Runner level | App permissions |
| --- | --- | --- |
| Organization | Organization | Self-hosted runners: write, Metadata: read |
| Personal account | Repository | Administration: write, Metadata: read |

**Warning:** Keep the app private key encrypted. With `Administration: write`, a person with the key can change and remove your repositories.

A free GitHub organization can use organization runners, but only in the Default runner group. If you can, use an organization, because its permission is smaller.

### Pools

A pool is a group of runners with the same sandbox size, image, and labels. Each pool is one GitHub runner scale set. A workflow selects a pool with the pool label in `runs-on`.

Example for a host with 12 threads and 64 GB of memory:

| Pool | Label | vCPU | Memory | Scratch disk | `min_idle` | `max_runners` |
| --- | --- | --- | --- | --- | --- | --- |
| Default | `runplane` | 2 | 8 GB | 64 GB | 1 | 5 |
| Large | `runplane-4cpu` | 4 | 16 GB | 64 GB | 0 | 2 |

The installer calculates the defaults from the host size. For an organization, runplane makes one scale set for each pool. For a personal account, runplane makes one scale set for each pool in each selected repository.

### The listener loop

Each scale set has one listener from `github.com/actions/scaleset/listener`. The listener sends a long poll to GitHub, and GitHub holds each poll for approximately 50 seconds. With each poll, runplane sends `maxCapacity`. This value is the number of sandboxes in the pool plus the number of new sandboxes that the host has resources for. Thus GitHub does not give the pool more jobs than the host can do. When a sandbox starts or stops, runplane calls `SetMaxRunners` on each listener with the new value.

For each message, the `Scale` function of the pool does these steps:

1. Call `AcquireJobs` for each `JobAvailable` message that the pool can take.
2. Record the `JobStarted` and `JobCompleted` messages in the job history.
3. Calculate the necessary runner count: `min(min_idle + TotalAssignedJobs, max_runners, capacity)`.
4. Start or stop sandboxes until the sandbox count agrees with the necessary count.

```go
func (p *pool) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg != nil {
		if ids := p.wanted(msg.JobAvailableMessages); len(ids) > 0 {
			if _, err := p.client.AcquireJobs(ctx, ids); err != nil {
				return err
			}
		}
		p.record(msg)
		p.stats = msg.Statistics
	}
	return p.resize(ctx)
}
```

The listener removes a message from the queue only after `Scale` returns without an error. If `Scale` returns an error, GitHub sends the message again. Thus `Scale` must give the same result when it gets the same message two times.

### Runners

Each sandbox gets a just-in-time (JIT) runner config from `GenerateJitRunnerConfig`. A JIT runner does one job and then exits. runplane does not write the JIT config to a disk. It sends the config to the sandbox through vsock (VM mode) or a Unix socket (container mode).

To stop an idle sandbox, runplane first removes the runner from GitHub. If GitHub tells runplane that the runner is busy, runplane keeps the sandbox. With this order, GitHub cannot give a job to a runner that runplane stops.

### Runner versions

Since 29 September 2026, GitHub does not send jobs to runners that are too old. Thus runplane updates the runner automatically:

- It examines the `actions/runner` releases one time each hour.
- It gets the end dates from `GET /repos/{owner}/{repo}/actions/runners/deprecations/{version}` or from the organization endpoint.
- It builds a new runner bundle and gives it to all new sandboxes.
- It shows a warning in the dashboard 14 days before an end date.

The end dates from the API are not always 30 days after the next release. Thus runplane must not calculate them from release dates.

### Cleanup loop

Sometimes runplane does not get a message, or a process stops. Thus runplane also compares the full state each minute and at startup:

| Found | Action |
| --- | --- |
| A sandbox with no database row | Stop the sandbox. |
| A database row with no sandbox | Set the row to `gone`. |
| A runner of a runplane scale set with no sandbox | Remove the runner from GitHub. |
| A sandbox that operates longer than its time limit | Stop the sandbox. |

## Sandboxes

### Lifecycle

1. The driver makes a scratch disk and the network for the sandbox.
2. The driver starts the sandbox: Firecracker in the jailer, or a Docker container with the `sysbox-runc` runtime.
3. `runplane-init` starts as the first process in the sandbox.
4. `runplane-init` gets the job config from runplane through vsock (VM mode) or a Unix socket (container mode).
5. `runplane-init` starts `dockerd` and the runner (`run.sh --jitconfig`) at the same time.
6. The runner connects to GitHub, and the sandbox state changes to `idle` (a warm sandbox).
7. GitHub gives a job to the runner, and the sandbox state changes to `busy`.
8. The runner completes the job and exits. Then `runplane-init` stops the sandbox.
9. The driver removes the sandbox, its scratch disk, and its network.

The sandbox states are `starting`, `idle`, `busy`, `stopping`, `gone`, and `failed`.

A restart or an update of runplane does not stop jobs. In VM mode, each Firecracker process operates in a dedicated systemd scope. In container mode, Docker keeps the containers. At startup, runplane finds the remaining sandboxes and connects to them again.

### Parts that the two modes share

| Part | VM mode | Container mode |
| --- | --- | --- |
| Image | runplane changes the OCI image into a read-only ext4 root disk | Docker uses the OCI image directly |
| Runner bundle | A read-only drive | A read-only bind mount |
| Writable runner folder | `runplane-init` puts an overlay on the runner bundle | The same |
| Scratch disk | A sparse ext4 file as a virtio drive | A sparse ext4 file on a loop device, attached at `/var/lib/docker` and the runner work folder |
| Config channel | vsock | A Unix socket |
| Network | One TAP device and one /30 subnet for each VM | One Docker bridge network, with traffic between containers off (`enable_icc=false`) |
| Logs | Serial console of the VM | Container log |

One base image operates in the two modes. The runner bundle, the network rules, and the caches are also the same.

Phase 0 must confirm the loop device scratch disk in container mode. If it does not operate, runplane measures the disk use of each container and stops a container that uses too much disk space.

### Images

| Item | Plan |
| --- | --- |
| Format | OCI images. The project publishes `ubuntu-24.04` and `ubuntu-26.04` base images to GHCR. |
| Custom images | A Dockerfile `FROM` a runplane base image. runplane uses the new image when the image digest changes. |
| Base contents | git, curl, jq, zstd, unzip, build-essential, Docker Engine with buildx and compose, Node.js LTS, Python 3, GitHub CLI, and a `runner` user with sudo |
| Base size | 3 GB or less. The GitHub-hosted image uses more than 50 GB. |

Pools use `ubuntu-24.04` by default. GitHub moves `ubuntu-latest` to Ubuntu 26.04 in late 2026, so the default can change to `ubuntu-26.04` after tests.

### VM mode

| Drive | Access | Contents | Shared |
| --- | --- | --- | --- |
| Root disk | Read-only | OS image as ext4 | Yes, by all VMs of the image |
| Runner bundle | Read-only | `actions/runner` with the cache patch | Yes, by all VMs |
| Scratch disk | Read-write | Overlay upper layer, `/var/lib/docker`, runner work folder | No. runplane erases it after the job. |
| Cache volume (Phase 5) | Read-write | Sticky cache of one repository | No. Each job gets a clone. |

runplane pulls the image and changes it into an ext4 root disk, so Docker is not necessary on a VM mode host. `runplane-init` makes one overlay with two lower layers: the runner bundle and the root disk. All writes go to the scratch disk. Docker data goes directly onto the scratch disk, because overlay2 cannot operate on an overlay mount.

runplane makes the scratch disk from an empty ext4 template file in milliseconds. All VMs read the same root disk file, so the host keeps one copy in its page cache.

Each VM gets the Firecracker entropy device, so TLS in the guest does not wait for random data at boot. The kernel `ip=` boot argument sets the guest address, so DHCP is not necessary.

The guest kernel is one 6.1 or 6.18 kernel with the Firecracker guest config, plus the options that Docker must have. The Moby `check-config.sh` script examines the config.

### Container mode

runplane uses the Docker Engine API with the `sysbox-runc` runtime. Sysbox must be version 0.7.1 or newer, because Docker 29.5 and newer use time namespaces.

Rules for the `sysbox` driver:

- If containers operate on the host, write the `bip` and `default-address-pools` values that Docker uses to `/etc/docker/daemon.json` before the Sysbox package install. Then the package only reloads Docker, and the containers continue to operate.
- Start one container at a time. Some container starts are not successful when many containers start together.
- Set `--cpus`, `--memory`, and `--pids-limit` on each container.
- Set the inner Docker daemon to the `overlay2` storage driver.
- Do not bind-mount host files with write access. Sysbox writes to bind-mounted host files as host root.
- Do not use `docker exec` for health checks. In issue #1018, calls into a stopped container used all host memory.
- Change only containers that have the runplane label. Docker on the host can also operate your other containers, for example Coolify apps.

The watchdog for issue #1018 examines the `sysbox-fs` connections in `/sys/fs/fuse/connections` each 10 seconds. If a connection stays blocked for more than 60 seconds, runplane writes `1` to the `abort` file of that connection. Then it removes the container with `docker rm -f` and starts a new one.

Phase 0 must also do a test of `--cgroup-parent=runplane.slice`, because Sysbox does not document it.

### Network

| Rule | Result |
| --- | --- |
| Masquerade from sandboxes to the internet | Sandboxes get internet access through the host address |
| Drop traffic between sandboxes | One job cannot attack a different job |
| Drop traffic from a sandbox to the host, except to the service address | Jobs can only connect to the cache server and the registry mirror |
| Drop traffic to private ranges and to `169.254.169.254` | Jobs cannot read cloud metadata or your private network |
| Drop packets with an incorrect source address | A sandbox cannot use the address of a different sandbox |

VM mode applies the rules to the TAP devices in a dedicated nftables table, `inet runplane`. Container mode applies them to the runplane Docker bridge. For forwarded container traffic, runplane puts its rules in the `DOCKER-USER` chain, because Docker reads that chain first. For traffic from containers to the host, runplane uses the same `inet runplane` table. runplane does not change other rules.

Firecracker rate limiters set a bandwidth limit for each VM. Sandboxes use public DNS servers by default. In the first version, sandboxes get only IPv4 egress.

**Caution:** In VM mode, if Docker also operates on the host with the iptables backend, runplane must add accept rules for the VM subnet to the `DOCKER-USER` chain. Docker sets the `FORWARD` policy to drop, and without these rules the VMs have no internet access.

### Resources

- All sandboxes get a lower CPU weight than other services, so CI does not make your production apps slower.
- runplane keeps a reserve for the host. The default reserve is 1 CPU and 2 GB of memory.
- The scheduler starts a sandbox only if the free memory is more than the sandbox memory. You can set the CPU overcommit. The default value is 1.0.
- The scheduler does not start a sandbox when the free disk space is less than 15%.
- Each job has a time limit. The default limit is 6 hours.

## Speed

### Time to start a job

| Step | Cold sandbox | Warm sandbox |
| --- | --- | --- |
| GitHub sends the job message | Less than 1 s | Less than 1 s |
| runplane gets a JIT config | Approximately 0.3 s | Done before the job |
| The sandbox starts | VM: 0.125 s or less. Container: approximately 1 s. | Done before the job |
| The runner starts and connects | 2–4 s | Done before the job |
| GitHub gives the job to the runner | Approximately 1 s | Approximately 1 s |
| **Total** | **4–7 s** | **1–2 s** |

Only the Firecracker boot time comes from a specification. The other values are estimates. Measure them in Phase 0.

### Warm sandboxes

Each pool keeps `min_idle` sandboxes started, with a registered runner. GitHub gives a new job to an idle runner immediately. A warm VM uses approximately 300 MB of host memory for the kernel, the runner, and `dockerd`. A warm container uses less, because it has no guest kernel. These values are estimates. In Phase 5, snapshot restore can make warm VMs share most of their memory.

### Caches

| Cache | Faster work | How it operates | Phase |
| --- | --- | --- | --- |
| Registry mirror | `docker pull`, `services:`, container jobs | A pull-through cache for Docker Hub on the host. All sandboxes use the host IP, and Docker Hub lets one IP do 100 anonymous pulls each 6 hours. | 3 |
| Actions cache server | `actions/cache`, `setup-*` caches, `cache-from: type=gha` | A cache v2 server on the host disk | 3 |
| Tool cache | `actions/setup-node`, `setup-python`, `setup-go` | The usual versions in `/opt/hostedtoolcache` in the base image | 3 |
| Sticky disks | Docker layers, BuildKit state, `node_modules` | One ext4 volume for each repository, with a copy-on-write clone for each job | 5 |

In the two modes, sandboxes get to the cache server and the registry mirror through one service address on the host.

The runner writes the GitHub value of `ACTIONS_RESULTS_URL` into each job. Thus a value in the runner `.env` file has no effect, and GitHub gives no way to set a custom cache endpoint. runplane patches the runner when it builds the runner bundle:

1. Download the official runner and make sure that its SHA-256 is correct.
2. In `bin/Runner.Worker.dll`, change the UTF-16 string `ACTIONS_RESULTS_URL` to `ACTIONS_RESULTS_ORL`.
3. In the runner `.env` file, set `ACTIONS_RESULTS_URL` to the local cache server.

After the patch, the runner writes the GitHub value to a different name, and the `.env` value stays. The local cache server sends all other results traffic to GitHub, for example artifact uploads. The falcondev cache server and Ubicloud use this patch or a forked runner. If the patch does not apply to a new runner release, runplane uses the runner without the patch. Then jobs use the GitHub cache.

The cache server keeps the caches of each repository isolated. It also uses the GitHub branch rules: a job reads from its ref, the base ref, and the default branch, and writes only to its ref. These rules prevent cache poisoning from pull requests. The server identifies the job from the sandbox source address and the job runtime token.

### Hardware

The CPU has the largest effect on build time. Dedicated vCPUs and dedicated servers are faster than shared vCPUs. On shared vCPUs, the speed changes with the load from other customers on the same server.

## Size and cost

| Item | Limit |
| --- | --- |
| runplane binary | Less than 30 MB |
| runplane memory when idle | Less than 50 MB |
| Host disk for one base image | 3 GB or less |
| Firecracker overhead for each VM | 5 MiB or less (specification) |
| Other services on the host | None in VM mode. Docker Engine and Sysbox in container mode. |

### Monthly prices

All prices are from 4 November 2026 and do not include VAT. 1 EUR = 1.1179 USD (ECB).

Services for Linux runners with 2 vCPU in private repositories. The prices include the free tier of each service:

| Service | Each minute | 5,000 minutes | 20,000 minutes | 50,000 minutes |
| --- | --- | --- | --- | --- |
| GitHub-hosted (Free plan, $12 included) | $0.006 | $18 | $108 | $288 |
| WarpBuild | $0.004 | $20 | $80 | $200 |
| Namespace (Team plan, $100 for 25,000 minutes) | $0.004 | $100 | $100 | $250 |
| Blacksmith (3,000 free minutes) | $0.004 | $8 | $68 | $188 |
| Ubicloud premium ($2.50 free credit) | $0.002 | $7.50 | $37.50 | $97.50 |
| runplane on your VPS | None | $0 | $0 | $0 |

In these host tables, "jobs at the same time" is the number of jobs with 2 vCPU and 4 GB after the host reserve, with no CPU overcommit.

Hosts without KVM (container mode):

| Host | vCPU and memory | Each month | Jobs at the same time |
| --- | --- | --- | --- |
| Contabo Cloud VPS 8 | 8 shared vCPU, 24 GB | €14.00 (€11.20 with 24 months) | 3 |
| Contabo Cloud VPS 16 | 16 shared vCPU, 64 GB | €37.00 | 7 |
| Netcup VPS 2000 | 8 shared cores, 16 GB | €26.01 (€19.23 with 24 months) | 3 |
| Netcup RS 2000 | 8 dedicated cores, 16 GB | €39.33 (€29.07 with 24 months) | 3 |
| Netcup RS 4000 | 12 dedicated cores, 32 GB | €75.21 | 5 |
| Hetzner Cloud CPX42 | 8 shared vCPU, 16 GB | €69.99 | 3 |
| Hetzner Cloud CCX23 | 4 dedicated vCPU, 16 GB | €86.49 | 1 |
| Linode 16 GB | 6 shared vCPU, 16 GB | $96 | 2 |
| Linode Dedicated, 4 vCPU (G7) | 4 dedicated vCPU, 8 GB | $86 | 1 |

These prices include one IPv4 address. These plans have no setup fee. Hetzner Cloud prices are for new orders since 15 June 2026. Servers that you already have keep their earlier price until you change the plan. The Hetzner CX plans have lower prices, but they were not available. Contabo shows different prices in some countries.

Hosts with KVM (VM mode):

| Host | CPU and memory | Each month | Setup fee | Jobs at the same time |
| --- | --- | --- | --- | --- |
| Contabo VDS S | 6 vCores (3 cores), 24 GB | €54.00 | €0 | 2 |
| OVHcloud RISE-1 | Xeon E-2386G, 6 cores, 32 GB | €56.99 | €56.99 (€0 if you pay 12 months in advance) | 5 |
| Hetzner server auction | i7-8700, 6 cores, 128 GB | From €76.70 | €0 | 5 |
| Hetzner AX42-1 | Ryzen 7 PRO 8700GE, 8 cores, 64 GB | €99.00 | €49 | 7 |
| Hetzner AX102-1 | Ryzen 9 7950X3D, 16 cores, 128 GB | €259.00 | €129 | 15 |
| DigitalOcean CPU-Optimized, Premium Intel | 4 vCPU, 8 GB | $109 | $0 | 1 |
| AWS c8i.xlarge, us-east-1 | 4 vCPU, 8 GB | $136.82, plus disk and IPv4 | $0 | 1 |

Hetzner dedicated prices include IPv4 (€1.70). Hetzner LTD models have lower prices, but they were out of stock. Auction prices change many times each day.

Above these monthly minute counts (approximate), the host price is less than the service price:

| Host | GitHub-hosted | Blacksmith | Ubicloud |
| --- | --- | --- | --- |
| Contabo Cloud VPS 8 (€14.00) | 5,000 | 7,000 | 9,000 |
| Netcup RS 2000 (€39.33) | 9,000 | 14,000 | 23,000 |
| Hetzner AX42-1 (€99.00) | 20,000 | 31,000 | 57,000 |

In December 2025, GitHub announced a fee of $0.002 for each self-hosted minute in private repositories. Then GitHub cancelled the start date of the fee and gave no new date. At this time, self-hosted minutes are free. If the fee starts, the price of a runplane minute is $0.002. This price is less than the price of a hosted minute.

## Security

| Threat | Control |
| --- | --- |
| A job tries to get access to the host | VM mode: KVM and the Firecracker jailer (chroot, dedicated UID, seccomp, cgroup). Container mode: a user namespace, the Sysbox seccomp filter, and no host Docker socket. A kernel bug can break container mode. |
| A job attacks runplane or other services on the host | The network rules let sandboxes connect only to the cache server and the registry mirror |
| A job attacks a different sandbox | The network rules drop traffic between sandboxes. In container mode, all containers share one ID mapping, so this control is weaker. |
| A job reads cloud metadata | The network rules drop traffic to `169.254.169.254` |
| A job reads files of a previous job | Each job gets a new scratch disk. The shared parts are read-only. |
| A pull request puts bad data in the cache | The cache server uses the GitHub branch rules |
| A job uses all CPU, memory, or disk | CPU and memory limits, a low CPU weight, a scratch disk limit, and a job time limit |
| Code from a fork of a public repository | Runner groups and personal-account pools accept only private repositories (default). In container mode, runplane rejects public repositories and fork pull requests, and you cannot change this rule. In VM mode, a job-start hook can reject jobs from fork pull requests. |
| A person makes the admin account before you | The first account is possible only with the one-time setup code from the install script |
| A person gets a copy of the database | runplane encrypts the app private key with a key in `/etc/runplane/secret.key` (mode 0600) |
| A person opens the dashboard | Password login (argon2id) with a rate limit. HTTPS with ACME when you set a domain. |

GitHub recommends that you use self-hosted runners only with private repositories. A new sandbox for each job does not prevent all risk: an attacker can use your network and CPU time. If you do not set a domain, open the dashboard through an SSH tunnel or Tailscale.

In the two modes, `/etc/runplane` and the runplane data folder (`/var/lib/runplane`, mode 0700) stay outside of all sandboxes.

## Data model

runplane keeps all state in one SQLite file in WAL mode: `/var/lib/runplane/runplane.db`.

| Table | Contents |
| --- | --- |
| `users`, `sessions` | Dashboard accounts and login sessions |
| `github_apps` | App ID, slug, client ID, encrypted private key, encrypted client secret |
| `installations` | Installation ID, account name, account type |
| `pools` | Label, scope, sandbox size, image, `min_idle`, `max_runners`, scale set ID |
| `images` | OCI reference, digest, root disk path (VM mode), status |
| `runner_bundles` | Runner version, patch status, end dates |
| `sandboxes` | Pool, mode, state, runner ID, runner name, IP, times, exit cause |
| `jobs` | GitHub job ID, repository, workflow, labels, event, queue time, start time, end time, result, sandbox |
| `cache_entries` | Repository, ref, key, version, size, last use |
| `settings`, `audit_log` | Configuration, the host mode, and user actions |

The scale set messages contain the repository, the workflow ref, the job name, the event, and the queue, assign, and finish times. Thus the job history is possible without the `Actions: read` permission.

## API for the frontend

The API uses JSON over HTTP. The dashboard gets live updates through one SSE stream. runplane makes an OpenAPI spec from the Go types, and the frontend makes TypeScript types from that spec. The dashboard can show sandboxes with the label "Runners".

| Method | Path | Action |
| --- | --- | --- |
| `POST` | `/api/setup` | Make the first admin account |
| `POST`, `DELETE` | `/api/session` | Start or stop a login session |
| `GET` | `/api/overview` | Active jobs, queue time, minutes in the last 24 hours, host use |
| `POST` | `/api/github/manifest` | Get the manifest form for GitHub |
| `GET` | `/api/github/callback` | Change the manifest code into app credentials (browser redirect) |
| `GET` | `/api/github/setup` | Installation callback (browser redirect) |
| `GET` | `/api/github/installations` | Installations and repositories |
| `GET`, `POST` | `/api/pools` | List pools, make a pool |
| `GET`, `PATCH`, `DELETE` | `/api/pools/{id}` | Read, change, or remove a pool |
| `GET` | `/api/sandboxes` | Sandboxes and their states |
| `POST` | `/api/sandboxes/{id}/stop` | Stop a sandbox |
| `GET` | `/api/sandboxes/{id}/log` | Serial console log (VM mode) or container log |
| `GET` | `/api/jobs` | Job history with filters and cursor pages |
| `GET` | `/api/jobs/{id}` | Job timeline, CPU use, and memory use |
| `GET`, `POST` | `/api/images` | List images, add a custom image |
| `GET`, `DELETE` | `/api/caches` | Cache use for each repository, remove caches |
| `GET` | `/api/host` | Host mode, KVM status, Docker and Sysbox versions, capacity, and `runplane doctor` results |
| `GET`, `PATCH` | `/api/settings` | Read and change the configuration |
| `GET` | `/api/events` | SSE stream |

The SSE event types are `sandbox.state`, `job.state`, `pool.stats`, `host.stats`, and `runner.version`.

## Repository layout

```text
cmd/runplane/                 serve, doctor, update, version
internal/api/
internal/github/
internal/scheduler/
internal/sandbox/             Driver interface, network rules, scratch disks
internal/sandbox/firecracker/
internal/sandbox/sysbox/
internal/image/
internal/cache/
internal/store/               schema, migrations, queries
init/                         runplane-init (static Go binary)
images/                       Dockerfiles for base images
kernel/                       guest kernel config and build script
web/                          frontend, embedded in the binary at build time
install.sh
```

## Phases

Most VPS plans have no KVM, and container mode has less code. Thus container mode comes first, and VM mode comes second.

### Phase 0: Tests on two hosts

On a VPS without KVM (Ubuntu 24.04), do these steps:

1. Install Docker Engine and Sysbox 0.7.1.
2. Start a Sysbox container from a Dockerfile image with `dockerd` and a JIT runner.
3. Do a workflow with `docker build`, a container job, and `services: postgres` with `ports:`.
4. Do a test of a loop device scratch disk at `/var/lib/docker` and of `--cgroup-parent`.
5. Start and stop 500 containers in a loop, and examine the host for the #1018 problem.

On a host with KVM, do these steps:

1. Boot a VM manually with Firecracker v1.17, a guest kernel, and an ext4 image from the same Dockerfile.
2. Do the same workflow in the VM.

On one of the two hosts, do these steps:

1. Operate the `dockerscaleset` example from `actions/scaleset`.
2. Make sure that a repository-level scale set operates on a personal account.
3. In each mode, measure the sandbox start time, the runner start time, and the memory of an idle sandbox.

Done when: the workflow passes in the two modes, and you have the measurements.

### Phase 1: First version, container mode

- Install script with the KVM check, the Docker and Sysbox setup, and `runplane doctor`
- Login, the one-time setup code, and HTTPS with ACME
- The GitHub App manifest flow for organizations and personal accounts
- Pools, scale set listeners, JIT runners, and warm sandboxes
- The `sysbox` driver with the network rules, the scratch disks, and the #1018 watchdog
- Automatic runner updates with the deprecations API
- Cleanup loop
- Job history and the live dashboard.

Done when:

- On a new VPS without KVM, a job passes in a container after the install, the GitHub connection, and a push.
- A runplane restart during a job does not stop the job.
- After a host reboot, runplane removes all remaining runners and sandboxes.

### Phase 2: VM mode

- The `firecracker` driver: jailer, TAP devices, nftables, overlay root, scratch disk, and vsock
- The ext4 root disks and the guest kernel build
- The mode selection at install, and a mode change in the configuration.

Done when: on a host with KVM, the install selects VM mode, and the Phase 1 tests pass in VMs.

### Phase 3: Speed

- Registry mirror, Actions cache server, and tool cache
- More pools and sizes, and custom images
- CPU and memory graphs for each job, with size advice.

Done when: with a warm sandbox, the median time from queue to job start is less than 3 s. A second `npm ci` with the local cache is faster than with the GitHub cache.

### Phase 4: Maintenance and safety

- Self-update of runplane from signed releases
- Daily SQLite backup (`VACUUM INTO`) with optional upload to S3
- Cache branch rules, fork pull request policy, and an optional list of permitted egress hosts
- Disk cleanup for caches and images, and an alert when the disk is almost full
- Audit log.

### Phase 5: More hosts and features

- More hosts: each host operates `runplane agent`, which opens the connection to the primary runplane, so SSH is not necessary
- Sticky disks
- Snapshot restore for warm VMs
- arm64 hosts
- An Incus driver as an alternative to Sysbox
- GitLab and Forgejo runners.

## Risks

| Risk | Effect | Plan |
| --- | --- | --- |
| `actions/scaleset` is in public preview | The interfaces can change | Pin the version. Keep `workflow_job` webhooks with the REST JIT endpoint as an alternative design. |
| Sysbox issue #1018 | Containers stop. In one report, the host also used all its memory. | The watchdog, one container start at a time, no `docker exec` health checks, and pinned versions of Sysbox and Docker that passed the Phase 0 tests |
| Sysbox support is best effort | Fixes come slowly | Keep the `Driver` interface small. Prepare the Incus driver. Recommend VM mode where KVM is available. |
| The Sysbox package install restarts Docker | Your other containers on the host restart | Write `bip` and `default-address-pools` before the install |
| Container mode shares the host kernel | A kernel bug can give a job access to the host | Private repositories only. VM mode for code that you do not trust. |
| The runner patch does not apply to a new release | No local Actions cache | Do a check of each new runner. If the patch does not apply, use the GitHub cache. |
| GitHub starts the self-hosted fee | $0.002 for each minute | The price stays less than the price of hosted runners |
| Nested virtualization | Slower I/O, and a live migration can cause a crash | Recommend bare metal for VM mode, or use container mode |
| The host kernel is not in the Firecracker test list (Ubuntu 24.04 uses 6.8) | Unknown problems | Do tests on Ubuntu 24.04, Debian 12, and Debian 13 in Phase 0 |
| Coolify v4.4.0 has a beta runner feature that also uses Sysbox | Users can select Coolify | runplane adds VM mode, operates without a public URL, works with personal accounts, and keeps warm sandboxes and local caches |

## References

- [Firecracker releases](https://github.com/firecracker-microvm/firecracker/releases)
- [Firecracker specification](https://github.com/firecracker-microvm/firecracker/blob/main/SPECIFICATION.md)
- [Sysbox](https://github.com/nestybox/sysbox)
- [Sysbox distribution compatibility](https://github.com/nestybox/sysbox/blob/master/docs/distro-compat.md)
- [Sysbox security](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/security.md)
- [Sysbox issue 1018](https://github.com/nestybox/sysbox/issues/1018)
- [Sysbox 0.7.1 and Docker time namespaces](https://github.com/nestybox/sysbox/issues/1028)
- [Docker Engine Go client](https://github.com/moby/moby/tree/master/client)
- [Incus instance options](https://linuxcontainers.org/incus/docs/main/reference/instance_options/)
- [actions/scaleset](https://github.com/actions/scaleset)
- [Runner scale sets](https://docs.github.com/en/actions/concepts/runners/runner-scale-sets)
- [Scale set app permissions](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api)
- [GitHub App manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
- [Runner version deprecations API](https://github.blog/changelog/2026-09-03-github-actions-early-september-2026-updates/)
- [Runner version enforcement](https://github.blog/changelog/2026-09-28-self-hosted-runner-version-enforcement-date-has-moved/)
- [Security of self-hosted runners](https://docs.github.com/en/actions/reference/security/secure-use)
- [GitHub Actions runner prices](https://docs.github.com/en/billing/reference/actions-runner-pricing)
- [Self-hosted fee announcement and postponement](https://github.blog/changelog/2025-12-16-coming-soon-simpler-pricing-and-a-better-experience-for-github-actions/)
- [falcondev cache server setup](https://gha-cache-server.falcondev.io/getting-started)
- [Docker Hub pull limits](https://docs.docker.com/docker-hub/usage/pulls/)
- [Docker in gVisor limits](https://gvisor.dev/docs/tutorials/docker-in-gvisor/)
- [Hetzner Cloud FAQ: no nested virtualization](https://docs.hetzner.com/cloud/servers/faq)
- [AWS EC2 nested virtualization](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html)
- [Oracle: Firecracker in a VM and on bare metal](https://blogs.oracle.com/cloud-infrastructure/firecracker-oci-vm-vs-bm)
- [Coolify GitHub Actions runners](https://coolify.io/docs/core/infrastructure/servers/github-actions-runners)
- [ubuntu-latest moves to Ubuntu 26.04](https://github.com/actions/runner-images/issues/14748)
- [Hetzner Cloud prices](https://www.hetzner.com/cloud/)
- [Netcup root servers](https://www.netcup.com/en/server/root-server)
- [Contabo VPS](https://contabo.com/en/vps/)
- [Linode plan prices (API)](https://api.linode.com/v4/linode/types)

