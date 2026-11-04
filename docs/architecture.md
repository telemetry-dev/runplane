# runplane architecture contract

This file is the contract for all runplane code. `BACKEND.md` gives the reasons and the plan. `GUI.md` and `gui/` give the visual design. The Paper file "Runplane" (file ID `01M6J03J0K5Q2XBZJ9256GN31J`) has the screen designs. If this file and `BACKEND.md` do not agree, this file is correct.

## 1. Scope

runplane is one Go binary. It operates GitHub Actions jobs in temporary sandboxes on one or more servers.

| Item | Decision |
| --- | --- |
| Go module | `github.com/runplane/runplane`, Go 1.27.3 (`github.com/actions/scaleset` v0.5.0 must have this version) |
| Release repository | Set at build time: `-ldflags -X github.com/runplane/runplane/internal/version.Repo=owner/name`. The default is `runplane/runplane`. |
| Primary | The server that has the dashboard, the database, and the GitHub credentials. Command: `runplane serve`. |
| Agent | A different server that operates sandboxes for the primary. Command: `runplane agent`. An agent has no database and no GitHub credentials. |
| Server | The primary or an agent. The UI word is "Server". The primary is also a server (kind `local`). |
| Pool | A runner label with a sandbox size and an image. The UI word is "Runner" or "label". One pool is one GitHub runner scale set (or one for each repository for personal accounts). |
| Sandbox | One temporary container (container mode) or one microVM (VM mode) for one job. |
| GitHub | runplane polls GitHub through the runner scale set API (`github.com/actions/scaleset` v0.5.0). runplane has no webhook. A public URL is not necessary. |
| GitHub App permissions | Organization: `organization_self_hosted_runners: write`, `actions: write`, `metadata: read`. Personal account: `administration: write`, `actions: write`, `metadata: read`. The `actions` permission gives job steps, logs, run data (branch, commit, actor), and re-runs. |
| Frontend | TanStack Start in SPA mode in `web/`. The build output goes to `internal/webui/dist`, and `go:embed` puts it in the binary. |
| API | JSON over HTTP, defined with Huma v2 (`github.com/danielgtaylor/huma/v2`) on `net/http`. `runplane openapi` writes the OpenAPI 3.1 spec. The frontend makes TypeScript types from the spec with `openapi-typescript` and calls the API with `openapi-fetch`. |

The design shows a webhook URL and "Last delivery" on the GitHub settings screen. runplane does not use webhooks. The UI shows the listener status at that location ("Listener: connected, last poll 12s ago").

## 2. Repository layout

```text
cmd/runplane/              The commands (section 12) and the connection of the concrete types
internal/version/          Version, Commit, Repo (set with -ldflags)
internal/config/           Config from flags and environment, file paths
internal/store/            SQLite schema, migrations, typed queries
internal/secret/           AES-256-GCM sealing with /etc/runplane/secret.key
internal/events/           In-process publish and subscribe bus (source of SSE)
internal/auth/             Passwords (argon2id), sessions, setup code, rate limit, roles
internal/api/              Huma API, handlers, SSE, static files, /install.sh
internal/webui/            go:embed of the frontend build (dist/)
internal/github/           App manifest flow, app JWT, installation tokens, REST calls, scale sets, listeners, runner releases
internal/github/fakegh/    Fake GitHub for tests, the end-to-end test, and `make fake`
internal/scheduler/        Pools, warm sandboxes, placement on servers, capacity, cleanup loop, job records
internal/sandbox/          Driver interface, shared types, scratch disks
internal/sandbox/control/  Control channel, host side (section 4.1)
internal/sandbox/netrules/ nftables and DOCKER-USER rules, TAP devices, TAP addresses (linux only)
internal/sandbox/sysbox/   Container mode driver (Docker Engine API, sysbox-runc)
internal/sandbox/firecracker/ VM mode driver (Firecracker, jailer, TAP, vsock)
internal/sandbox/devdocker/ Development driver: Docker with runc and --privileged. Never for production. Only with RUNPLANE_DEV_INSECURE=1.
internal/image/            OCI pulls, local builds, ext4 root disks (VM mode), runner bundles (download, SHA-256, patch)
internal/cache/            Actions cache v2 server and Azure blob emulation, results proxy
internal/cachestore/       Cache index and statistics in the store, and the resolver that finds the sandbox and the refs of a request (section 5.2)
internal/mirror/           Docker Hub pull-through registry mirror
internal/host/             Host facts (CPU, memory, disk, KVM, versions), doctor checks, host stats
internal/agent/            Agent protocol: primary side (RemoteDriver, listener) and agent side
internal/sshinstall/       Install over SSH (golang.org/x/crypto/ssh)
internal/shell/            Debug shell: browser terminal (WebSocket) and SSH gateway
internal/notify/           Notification channels (Slack, Discord, generic webhook)
internal/backup/           VACUUM INTO, retention, S3 upload (SigV4, no SDK)
internal/devseed/          Demo data for an empty database (`runplane dev-seed`). Only in builds with the tag `devseed`.
internal/update/           Self-update from signed releases (ed25519)
internal/tlsutil/          ACME (autocert) and the self-signed agent certificate
internal/initbin/          go:embed of the runplane-init binary for the same arch (internal/initbin/bin/runplane-init)
init/                      runplane-init (static Go binary, package main)
e2e/                       End-to-end tests: dev mode (`make e2e`), and container and VM mode on a Linux host (`e2e/linux/run.sh`, in Lima: `e2e/linux/lima.sh`)
images/ubuntu-24.04/       Dockerfile for the base image
images/ubuntu-26.04/       Dockerfile for the base image
kernel/                    Guest kernel config fragment and build.sh
web/                       Frontend (TanStack Start, pnpm)
install.sh                 Installer for the primary and for agents
Makefile                   build, web, test, lint, release targets
.github/workflows/         ci.yml, release.yml, images.yml, kernel.yml
```

Rules for packages:

- Linux-only code uses `//go:build linux`. Each such package also has a small `_other.go` file. Thus `go build ./...` and `go vet ./...` pass on macOS.
- If a package uses the behavior of a different package, it declares a small interface at that location. Only `cmd/runplane` connects concrete types. Thus `scheduler` does not import `github`, and `api` does not import `scheduler`. `github` imports `scheduler` for its interface types.
- Logging: `log/slog`, with a logger as a parameter. No global loggers.
- Errors: wrap with `fmt.Errorf("verb noun: %w", err)`. No panics in request paths.
- IDs: ULID strings (`github.com/oklog/ulid/v2`) in lowercase for all runplane rows. GitHub IDs stay int64.
- Times: UTC. SQLite stores INTEGER unix milliseconds. JSON uses RFC 3339 strings.
- Tests: standard `testing`, table tests where useful. A test must find a real defect. Do not write a test that only repeats the implementation.

## 3. Files and configuration

| Path | Mode | Contents |
| --- | --- | --- |
| `/etc/runplane/runplane.env` | 0600 | Environment for the systemd unit |
| `/etc/runplane/secret.key` | 0600 | 32 random bytes. Key for `internal/secret`. |
| `/etc/runplane/agent.json` | 0600 | Agent only: `{"primary":"host:port","fingerprint":"sha256:<hex>","token":"rp_srv_...","server_id":"","secret":""}`. install.sh writes the first three fields. The agent writes `server_id` and `secret` after it registers and removes `token`. |
| `/var/lib/runplane/` | 0700 | Data folder |
| `/var/lib/runplane/runplane.db` | 0600 | SQLite, WAL mode, primary only |
| `/var/lib/runplane/sandboxes/<id>/` | 0700 | Scratch disk, control socket, console log for one sandbox. In VM mode, also the record of the driver (`vm.json`). |
| `/var/lib/runplane/images/` | 0700 | ext4 root disks and temporary pull folders (VM mode) |
| `/var/lib/runplane/runners/<version>-<arch>/` | 0755 | Patched runner bundles |
| `/var/lib/runplane/cache/` | 0700 | Actions cache blobs (primary) |
| `/var/lib/runplane/mirror/` | 0700 | Registry mirror blobs (each server) |
| `/var/lib/runplane/logs/` | 0700 | Job logs (primary), `<job-id>.log` |
| `/var/lib/runplane/backups/` | 0700 | Database backups |
| `/var/lib/runplane/tls/` | 0700 | ACME cache and the agent certificate |
| `/var/lib/runplane/kernel/vmlinux` | 0644 | Guest kernel (VM mode). install.sh puts it there. An update replaces it with `vmlinux-<arch>` of the release (section 11). |
| `/var/lib/runplane/scratch-templates/` | 0700 | Empty ext4 templates for scratch disks, one for each size |
| `/etc/subuid`, `/etc/subgid` | 0644 | VM mode: the line `runplane-vm:1879048192:16384` in each file. install.sh reserves these uids and gids for the VMs (section 4.3). |
| `/etc/modules-load.d/runplane.conf` | 0644 | VM mode: the kernel module `tun`, which systemd loads at boot. install.sh writes the file, and the uninstall removes it. |

Environment variables (`internal/config`). A flag with the same name in lowercase overrides the variable.

| Variable | Default | Use |
| --- | --- | --- |
| `RUNPLANE_LISTEN` | `:8484` | Dashboard and API |
| `RUNPLANE_AGENT_LISTEN` | `:8485` | TLS listener for agents (primary) |
| `RUNPLANE_SERVICE_PORT` | `8486` | Cache server and registry mirror for sandboxes (each server) |
| `RUNPLANE_DATA_DIR` | `/var/lib/runplane` | Data folder |
| `RUNPLANE_CONFIG_DIR` | `/etc/runplane` | Configuration folder |
| `RUNPLANE_MODE` | from install | `container`, `vm`, or `dev` (the `devdocker` driver) |
| `RUNPLANE_DEV` | `0` | `1` sets relaxed cookies and CORS for the Vite dev server |
| `RUNPLANE_DEV_INSECURE` | `0` | `1` lets the `devdocker` driver operate |
| `RUNPLANE_SSH_LISTEN` | `:2222` | SSH gateway for debug shells (primary). install.sh sets `:2222`. |
| `RUNPLANE_GITHUB_API_URL` | empty | GitHub REST API. Empty means `https://api.github.com`. Tests set the fake GitHub, for example `http://127.0.0.1:9100/api/v3`. |
| `RUNPLANE_GITHUB_URL` | empty | GitHub web address. Empty means `https://github.com`. |
| `RUNPLANE_DEV_RUNNER_DIR` | empty | One runner folder for all runner versions, not patched. Container and dev mode only. For tests. |

The `settings` table holds most settings (section 6), not the environment.

## 4. Sandbox driver contract (`internal/sandbox`)

```go
package sandbox

type Mode string // "container", "vm", or "dev"

type State string

const (
	StateStarting State = "starting" // sandbox starts, runner not connected yet
	StateIdle     State = "idle"     // runner listens for a job (warm sandbox)
	StateBusy     State = "busy"     // runner has a job
	StateStopping State = "stopping"
	StateGone     State = "gone"     // removed
	StateFailed   State = "failed"   // start or runtime failure; then removed
)

type Spec struct {
	ID            string            // ULID, also the sandbox host name suffix
	PoolID        string
	Image         ImageRef          // resolved image for this server
	RunnerVersion string            // for example "2.330.0"
	Arch          string            // "x64" or "arm64"
	VCPU          int
	MemoryMB      int
	DiskGB        int
	TimeLimit     time.Duration     // the job time limit, from the job start; sandbox.Deadline
	JITConfig     string            // encoded JIT config; never written to disk
	Env           map[string]string // more runner environment (cache URL and others)
	Debug         bool              // keep the sandbox after the job for a debug shell
	Labels        map[string]string // runplane labels; the driver adds them to the container or VM
}

type ImageRef struct {
	Ref    string // OCI reference
	Digest string // sha256:...
}

type Sandbox struct {
	ID        string
	PoolID    string
	State     State
	IP        string
	CreatedAt time.Time
	Spec      Spec // JITConfig empty after start
}

type Capacity struct {
	Mode            Mode
	Arch            string
	VCPU            int   // total
	MemoryMB        int   // total
	DiskBytes       int64 // total of the data disk
	FreeDiskBytes   int64
	AllocVCPU       int   // sum of operating sandboxes
	AllocMemoryMB   int
	ReserveVCPU     int   // from settings
	ReserveMemoryMB int
	Overcommit      float64 // CPU overcommit factor from settings
	MaxSandboxes    int     // 0 = no limit
}

// Fits reports if a new sandbox of this size can start now.
func (c Capacity) Fits(vcpu, memoryMB int) bool

type EventType string

const (
	EventState   EventType = "state"   // State and Reason set
	EventRunner  EventType = "runner"  // runner lifecycle from runplane-init (Runner set)
	EventMetrics EventType = "metrics" // Metrics set
)

type Event struct {
	SandboxID string
	Type      EventType
	Time      time.Time
	State     State
	Reason    string
	Runner    *RunnerEvent
	Metrics   *Metrics
}

type RunnerEvent struct {
	Kind     string `json:"kind"`               // "listening", "job.started", "job.completed", "exited", "resume"
	JobName  string `json:"job_name,omitempty"`
	Result   string `json:"result,omitempty"`   // for job.completed
	ExitCode int    `json:"exit_code,omitempty"` // for exited
	Error    string `json:"error,omitempty"`     // for exited: the failure of the runner (section 4.2, step 6)
}

type Metrics struct {
	CPUPercent float64 // of the sandbox vCPU allocation, 0 to 100
	MemoryMB   int
}

type Driver interface {
	Mode() Mode
	Start(ctx context.Context, spec Spec) error // returns when the sandbox process starts; state goes to starting
	Stop(ctx context.Context, id string) error  // idempotent
	List(ctx context.Context) ([]Sandbox, error)
	Capacity(ctx context.Context) (Capacity, error)
	Events() <-chan Event
	Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) // console and runner log of the sandbox
	Shell(ctx context.Context, id string, size TermSize) (Terminal, error)    // debug shell
	Hold(ctx context.Context, id string, until time.Time) error                // keep a finished debug sandbox until this time
	Close() error
}

type TermSize struct{ Rows, Cols uint16 }

type Terminal interface {
	io.ReadWriteCloser
	Resize(TermSize) error
}
```

Rules for all drivers:

- A driver changes only the containers, VMs, networks, and files that have the runplane label (`dev.runplane.sandbox=<id>`) or that are in its own folders.
- The container mode and dev drivers also put the label `dev.runplane.data=<data folder>` on their containers, volumes, and probe containers. Each Docker query of the driver uses this label as a filter. Thus two runplane processes on one Docker do not change the sandboxes of the other process.
- After a restart, `List` returns the sandboxes that still operate. The driver connects to their control channels again.
- In container mode, `Start` starts one sandbox at a time (one mutex).
- The driver sends `EventState` for each state change. It also sends `EventMetrics` each 5 seconds for each sandbox in the `busy` state.
- After the job, `runplane-init` stops the sandbox. The driver then removes the sandbox and sends `StateGone`.
- A driver with a `Configure(sandbox.Settings)` method gets the dashboard settings of its sandboxes. The fleet sends them to each server when they change.

### 4.1 Control channel (host side in `internal/sandbox/control`, guest side in `init/`)

Each sandbox has one control channel between `runplane-init` (guest) and the server that operates the sandbox (host).

| Mode | Guest side | Host side |
| --- | --- | --- |
| Container and dev | Unix socket `/run/runplane/control.sock`. The host folder `/var/lib/runplane/sandboxes/<id>/ctl` has a read-only bind mount at `/run/runplane`. | The host listens on `/var/lib/runplane/sandboxes/<id>/ctl/control.sock`. |
| VM | vsock: connect to CID 2, port 1024 | The Firecracker `uds_path` is `<chroot>/v.sock`. The host listens on `<chroot>/v.sock_1024`. |

Dev mode (the `devdocker` driver on macOS): a Unix socket in a bind mount does not cross the Linux VM of Docker Desktop or OrbStack. Thus the host also listens on one TCP port (`RUNPLANE_SERVICE_PORT` + 1, default 8487). For each sandbox, the host writes the file `<sandbox dir>/ctl/control.addr` with one line: `tcp://host.docker.internal:8487 <sandbox-id> <token>`. If this file exists, the guest dials the TCP address and sends the line `RUNPLANE <sandbox-id> <token>\n` before yamux starts. The host closes the connection if the token is not correct. Container mode and VM mode do not use TCP.

The scratch disk is mounted at `/mnt/scratch` in all modes. In container mode, the driver attaches the scratch disk (loop device) on the host and bind-mounts it at `/mnt/scratch`. In dev mode, a Docker volume is the scratch disk. runplane-init uses `/mnt/scratch/docker` for `/var/lib/docker` and `/mnt/scratch` for the upper layer of the runner folder.

The guest always dials. On the connection, the two sides use `github.com/hashicorp/yamux`. The guest is the yamux client. Each side serves HTTP/1.1 on the streams that the other side opens.

Guest to host (the host serves):

| Method and path | Body | Result |
| --- | --- | --- |
| `GET /v1/config` | | `GuestConfig` JSON |
| `POST /v1/events` | `RunnerEvent` JSON | 204 |
| `POST /v1/logs` | Lines of text, streamed (chunked). Each line: `<unix-ms> <source> <text>`. The source is `runner`, `diag`, `init`, or `dockerd`. | 204 at the end of the stream |

Host to guest (the guest serves):

| Method and path | Body | Result |
| --- | --- | --- |
| `POST /v1/hold` | `{"until":"RFC3339"}` | 204. After the runner exits, init waits until this time, then stops. |
| `POST /v1/shutdown` | | 204. Init stops the runner and powers off. |
| `POST /v1/shell?rows=&cols=` | | `101 Switching Protocols` to a raw stream. Bytes on the stream go to a PTY with a login shell as `runner`. The first response header `X-Runplane-Session` gives a session ID. |
| `POST /v1/shell/resize?session=&rows=&cols=` | | 204 |

```go
type GuestConfig struct {
	SandboxID        string            `json:"sandbox_id"`
	Hostname         string            `json:"hostname"`
	JITConfig        string            `json:"jit_config"`
	RunnerDir        string            `json:"runner_dir"` // read-only runner bundle in the guest
	Env              map[string]string `json:"env"`        // written to the runner .env
	DNS              []string          `json:"dns"`
	RegistryMirror   string            `json:"registry_mirror"` // http://<service-ip>:<port>, empty = none
	TimeLimitSeconds int               `json:"time_limit_seconds"`
	Debug            bool              `json:"debug"`
}
```

The guest tries the connection again each second, with no limit. If runplane restarts during a job, the job continues. After the guest connects again, it sends `POST /v1/events {"kind":"resume"}` and continues to stream logs. The guest asks for `/v1/config` one time only. The JIT config is only in memory. Thus, if the guest asks for its config after a runplane restart, the sandbox cannot run a job. The host then answers 409 and stops the sandbox. The sandbox ends as `gone`, and the scheduler starts a new sandbox.

### 4.2 runplane-init steps

1. In VM mode: mount `/proc`, `/sys`, `/dev`, `/dev/pts`, `/dev/shm`, `/sys/fs/cgroup` (cgroup2), `/run`, and `/tmp`. Mount the scratch disk (`/dev/vdc`) at `/mnt/scratch`. After step 2, set the host name, `/etc/hostname`, `/etc/hosts`, and `/etc/resolv.conf`. `/etc/hosts` gets the line `127.0.1.1 <host name>`. If no line of the image names `localhost`, it also gets the `localhost` lines that Docker writes, because a root disk from `docker export` has an empty `/etc/hosts`. In container mode, Docker does these steps.
2. Connect the control channel. Get `GuestConfig`.
3. Prepare the runner folder `/home/runner/actions-runner` as an overlay. The lower layer is the read-only runner bundle. The upper layer and the work folder are on scratch. Write `.env` from `Env`.
4. Start `dockerd` (storage driver `overlay2`, data root `/var/lib/docker` on scratch, registry mirror from the config). At the same time, start the runner as user `runner`: `./run.sh --jitconfig <JITConfig>`. Do not let dockerd delay the runner start.
5. Read the runner stdout. Send `listening` on "Listening for Jobs", `job.started` on "Running job: <name>", and `job.completed` on "Job <name> completed with result: <result>". Stream all lines to `/v1/logs` with the source `runner`. Tail `_diag/pages/*.log` and stream these lines with the source `diag`.
6. When the runner exits, send `exited`. If the runner has a failure, the event has an `error`. The error is "the runner did not start; see the sandbox log" if init cannot start the runner. It is "the runner did not connect to GitHub" if the runner exits by itself before "Listening for Jobs". The exit code does not count, because the runner exits with code 0 after a bad JIT config. If runplane stops the runner, or init stops it at the time limit, the runner has no failure. With an `error`, the driver sets the sandbox to `failed` with the error as the reason. In container mode, init then exits with code 1. If a hold is set, wait until the hold time. Then stop dockerd, sync, and power off. On VM mode x86_64, init uses `reboot(RB_AUTOBOOT)` with the kernel argument `reboot=k`, because Firecracker exits on a CPU reset but not on a power off. On arm64 it uses `reboot(RB_POWER_OFF)`. In container mode, PID 1 exits.
7. At the time limit after the job start, kill the runner and continue at step 6. Before a job, the runner waits at most the time limit plus `sandbox.IdleGrace` (5 minutes).

### 4.3 VM mode driver (`internal/sandbox/firecracker`)

- Each VM operates in the systemd scope `runplane-vm-<id>.scope` in `runplane.slice`. In the scope, the jailer starts Firecracker in a chroot under `/var/lib/runplane/jail/`.
- Each VM has a dedicated uid and gid. The uid is the start of the `runplane-vm` range in `/etc/subuid` plus `n`, the index of the TAP device `rp-<n>`. The gid is the start of the range in `/etc/subgid` plus `n`. The jailer, the TAP owner and group (`TUNSETOWNER`, `TUNSETGROUP`), and the scratch disk use these IDs. Thus the Firecracker process of a VM cannot send signals to, trace, or open the files of a different VM. After a restart, the driver calculates the IDs again from the TAP index in the record. If the range has no ID for the TAP index, `Start` returns an error.
- The record `<sandbox dir>/vm.json` holds the spec without the JIT config, the TAP index, `created_at`, the state, `job_started`, `hold_until`, `runner_exit`, and `runner_error`. The driver writes the record at each change. With `runner_exit`, a VM that powers off after a runplane restart ends as `gone`, not as `failed`. With `runner_error` (the `error` of the `exited` event), the VM ends as `failed` with that reason.
- The driver stops a VM 1 minute after `sandbox.Deadline(created_at, job_started, TimeLimit)`, or 1 minute after the end of a debug hold if that time is later. Before that, runplane-init stops the runner (section 4.2, step 7).
- The guest kernel command line is `console=ttyS0 quiet reboot=k panic=1 init=/sbin/runplane-init runplane.mode=vm ip=<guest>::<host>:255.255.255.252::eth0:off random.trust_cpu=on`. Firecracker adds `root=/dev/vda ro` for the root drive, and `pci=off` from v1.13.
- Each scope has the property `Before=<unit>`, where `<unit>` is the systemd service of runplane from `/proc/self/cgroup`. Thus, when systemd stops the two units, as at a host shutdown, it stops runplane first. In `Close`, the driver reads the jobs of systemd (`systemctl list-jobs`). If a VM scope has a stop job, the driver tells runplane-init in that VM to power off, as `Stop` does, and waits until the VM is gone. The runner then cancels its job. A restart or a stop of runplane alone does not stop the VMs.
- In VM mode, the doctor of the running service does the sandbox internet check with `Driver.Probe`. The probe does not boot a VM. It puts a network namespace behind a veth with a free TAP name and address, and with the firewall rules of a VM. From the namespace, it gets `https://api.github.com` through the first IPv4 DNS server of the settings. Thus the check examines the host part of the VM network: IP forwarding, NAT, the runplane rules, and other firewalls of the host.

### 4.4 IP forwarding

The traffic of the sandboxes goes through the host, thus IPv4 forwarding must be on. runplane sets `net.ipv4.ip_forward` to 1 when it starts (`netrules.Manager.Setup`). runplane does not use a sysctl file at boot. The reason is the `FORWARD` chain of iptables. Docker sets the policy of that chain to `DROP` only if Docker starts the forwarding itself. If forwarding is on before Docker starts, the policy stays `ACCEPT`, and the server forwards packets between all of its networks.

Thus the unit of runplane has `After=docker.service` in all modes. On a server with Docker, Docker starts first at each boot. Docker sets `net.ipv4.ip_forward` to 1 and the `FORWARD` policy to `DROP`. Then runplane adds its accept rules for the sandbox traffic at the top of `FORWARD` and in `DOCKER-USER`.

In VM mode without Docker, no program sets the `FORWARD` policy. runplane sets `net.ipv4.ip_forward` to 1, and the policy stays as the host firewall sets it. On a host without a firewall, the policy is `ACCEPT`. The table `inet runplane` controls the traffic of the `rp-*` TAP devices, but not the traffic between other interfaces. On a server with more than one network, set a drop policy for forwarded traffic in the host firewall. For example, with ufw, use `ufw default deny routed`. runplane accepts the traffic of the `rp-*` interfaces at the top of `FORWARD`.

The doctor and the Linux end-to-end tests examine the same items:

| Check | Result |
| --- | --- |
| Doctor check `ip_forward` | A failure if forwarding is off. The check tells the admin to restart runplane. |
| Doctor check `firewall` | A warning if Docker is installed, forwarding is on, and the `FORWARD` policy is `ACCEPT`. In this condition, forwarding was on before Docker started. |
| `afterReboot` in `e2e/container_test.go`, for container and VM mode | Forwarding is on, the `FORWARD` policy is `DROP`, and the doctor gives `ok` for `ip_forward` and `firewall`. |

## 5. Agent protocol (`internal/agent`)

- The primary listens with TLS on `RUNPLANE_AGENT_LISTEN`. The certificate is self-signed (ECDSA P-256, 10 years). The primary makes it at the first start and keeps it in `/var/lib/runplane/tls/agent.{crt,key}`. The fingerprint is the SHA-256 of the DER certificate, written as `sha256:<hex>`.
- The agent dials the primary and pins the fingerprint. It does not use the system CA pool.
- The first message from the agent is one JSON line: `{"token":"rp_srv_...","server_id":"","secret":"","version":"","host":HostInfo}`. A new agent sends a one-time install token. A known agent sends its server ID and secret.
- The reply from the primary is one JSON line: `{"ok":true,"server_id":"...","secret":"..."}` or `{"ok":false,"error":"..."}`. The agent writes a new secret to `/etc/runplane/agent.json`.
- Then the two sides use yamux on the TLS connection. The primary is the yamux client. Each side serves HTTP on streams.

Primary to agent (the agent serves): `POST /v1/sandboxes` (Spec), `DELETE /v1/sandboxes/{id}`, `GET /v1/sandboxes`, `GET /v1/capacity`, `GET /v1/sandboxes/{id}/logs?follow=1`, `POST /v1/sandboxes/{id}/hold`, `GET /v1/sandboxes/{id}/shell` (upgrade), `GET /v1/host` (HostInfo and doctor results), `POST /v1/images/ensure` (ImageRef, returns the digest when ready), `POST /v1/images/gc` (`{"keep":["sha256:..."]}`, removes the images that are not in `keep`), `POST /v1/runners/ensure` (version, arch), `POST /v1/runners/prune` (`{"keep":["2.330.0"]}`, removes the runner bundles except the two newest and the versions in `keep`), `GET /v1/mirror/stats` (counts of the registry mirror of the agent), `POST /v1/settings` (driver settings), `POST /v1/update` (version).

Agent to primary (the primary serves): `POST /v1/events` (ndjson stream of `sandbox.Event` values and host stats each 10 seconds), and `ANY /v1/cache/*`. The agent cache endpoint sends Actions cache requests to the primary cache server with the header `X-Runplane-Sandbox: <id>`.

`agent.RemoteDriver` implements `sandbox.Driver` on the primary with these calls. The JIT config goes to the agent only inside the `Start` call. The agent keeps it in memory.

If an agent has no connection for more than 60 seconds, its status is `unreachable`. When an agent connects again, the scheduler compares the state (section 8.4).

An agent must operate the release of the primary, because the two sides must speak the same protocol. When an agent connects with a different version, the fleet sends `POST /v1/update` with the version of the primary. The `auto_update` setting does not apply to this update. The agent replaces its binary, restarts, and connects again. Its sandboxes continue. The fleet tries each version one time for each agent. After the primary updates, it restarts, thus the agents connect again and get the new release. The primary updates no agent if it is not a release build (for example `dev`). An admin can start the update again with `POST /api/servers/{id}/update`. The update is complete when the agent connects with the new version. If the update returns an error, or if the agent is `unreachable` after the update, the update is not successful, and the status detail of the server tells why. The `update_state` of the server API object shows the state: `current`, `available`, `updating`, `failed`, or `none`.

### 5.1 Service port

Each server serves one HTTP listener on `RUNPLANE_SERVICE_PORT` for its sandboxes. The path `/v2/` goes to the registry mirror (`internal/mirror`). All other paths go to the Actions cache server (`internal/cache`), because `@actions/cache` drops a path in the URL. The cache server sends the paths that are not for the cache to the GitHub results service. It sends only the requests of a sandbox, of the host itself, or of the agent tunnel. The listener uses all addresses, thus without this rule anyone could send requests to GitHub through the server. In container mode and VM mode, the `input` chain of the nftables table `inet runplane` also drops TCP packets to the service port that come in on an interface that is not a sandbox interface and not `lo`. Thus only sandboxes and the host itself can connect to the service port. The rule does not change the agent port or the dashboard port. On an agent, the cache requests go through the yamux tunnel to the primary. The mirror operates on each server. The listener has no read or write timeout, only `ReadHeaderTimeout`, because cache blobs can be several GB.

The fleet pulls a ready image with a tag again each day on each server, because the builtin images get a new build each week; the image stays ready while it does, and a failed pull keeps the old image. The fleet removes unused images on all connected servers: after a new digest of an image on a server, and when an admin removes an image. In VM mode, the image manager also removes the root disks of an earlier runplane-init once after a start. `GET /api/caches` shows the sum of the mirror counts of the primary and of the connected agents.

### 5.2 Actions cache refs

The cache server takes the repository and the refs of a request from the records of runplane. It does not use the `ac` claim of the runtime token of the job. runplane cannot examine the signature of that token. Thus a job can write a different claim into the token. runplane sends the token only to the GitHub results service, for the requests that are not for the cache.

Before the scheduler acquires a job, it reads the run (section 7). It records `event`, `head_branch`, `pr_number`, and `base_ref` on the job row. The resolver in `internal/cachestore` gives the refs from these values:

| Event of the run | Ref | Base ref |
| --- | --- | --- |
| `pull_request`, `pull_request_review`, `pull_request_review_comment`, with a known pull request number | `refs/pull/<n>/merge` | `refs/heads/<base_ref>` |
| A pull request with no known number (GitHub shows no pull request for a fork), `pull_request_target`, `workflow_run` | none | none |
| `release`, or a run whose `workflow_ref` ends in `@refs/tags/<head_branch>` | `refs/tags/<head_branch>` | none |
| All other events | `refs/heads/<head_branch>` | none |

A job saves entries only to its ref. A job restores entries from its ref, then from its base ref, then from the default branch of the repository. A job with no ref can only restore from the default branch.

## 6. Data model (`internal/store`)

SQLite with `modernc.org/sqlite`, WAL mode, `foreign_keys=ON`, `busy_timeout=5000`. Migrations are numbered SQL files in `internal/store/migrations/`, embedded in the binary. One writer connection and a pool of reader connections.

```sql
settings(key TEXT PRIMARY KEY, value TEXT NOT NULL)          -- JSON values
users(id, email UNIQUE, name, password_hash, role, created_at, last_login_at)          -- role: admin | member
sessions(id_hash PRIMARY KEY, user_id, created_at, expires_at, last_seen_at, ip, user_agent)
invites(id, email, role, token_hash, created_by, created_at, expires_at, accepted_at)
setup_codes(code_hash PRIMARY KEY, created_at, expires_at, used_at)
github_apps(id, app_id INTEGER, slug, name, client_id, owner_login, owner_type, html_url,
            private_key_enc BLOB, client_secret_enc BLOB, webhook_secret_enc BLOB, created_at)
manifest_states(state_hash PRIMARY KEY, account_type, organization, created_at, expires_at)
installations(id INTEGER PRIMARY KEY /* GitHub installation ID */, app_id, account_login, account_id, account_type /* Organization | User */,
              avatar_url, repository_selection /* all | selected */, suspended_at, created_at, updated_at)
repositories(id INTEGER PRIMARY KEY, installation_id, full_name, private, archived, default_branch, updated_at)
images(id, name, ref, is_default, source /* registry | builtin */, created_at, updated_at)
image_builds(image_id, server_id, digest, status /* pending | pulling | building | converting | ready | failed */, error, size_bytes,
             path /* VM mode: root disk */, updated_at,
             PRIMARY KEY(image_id, server_id))
pools(id, name, label UNIQUE, arch, vcpu, memory_mb, disk_gb, image_id, min_idle, max_runners,
      time_limit_minutes, server_ids /* JSON array, empty = all */, enabled, created_at, updated_at)
scale_sets(id, pool_id, installation_id, repository_id /* NULL for organization */, github_id INTEGER, runner_group_id,
           name, status /* creating | listening | error | deleting */, error, last_poll_at, stats_json, updated_at)
servers(id, name, kind /* local | agent */, status /* online | installing | unreachable | draining | disabled */, status_detail,
        address, provider, region, os, kernel, arch, mode, vcpu, memory_mb, disk_bytes, disk_kind, kvm, docker_version, sysbox_version,
        firecracker_version, version, secret_hash, last_seen_at, created_at, updated_at)
server_doctor(server_id PRIMARY KEY, checked_at, checks_json)          -- last doctor result of each server
server_tokens(id, token_hash, name, created_by, created_at, expires_at, used_at, server_id)
runner_bundles(version, arch, sha256, patched, status, path, error, end_date, released_at, created_at, PRIMARY KEY(version, arch))
sandboxes(id, pool_id, server_id, scale_set_id, mode, state, runner_id INTEGER, runner_name, ip, image_digest, runner_version,
          job_id, debug, hold_until, vcpu, memory_mb, disk_gb, created_at, idle_at, busy_at, stopped_at, exit_reason, error)
jobs(id, runner_request_id INTEGER UNIQUE, github_job_id INTEGER, scale_set_id, pool_id, sandbox_id, server_id,
     repository, workflow_ref, workflow_file, workflow_name, job_name, run_id INTEGER, run_number INTEGER, run_attempt INTEGER,
     head_branch, head_sha, commit_message, event, actor, pr_number, base_ref, labels_json, state /* queued | assigned | running | completed */,
     result /* succeeded | failed | cancelled | skipped | NULL */, reject_reason, queued_at, assigned_at, started_at, finished_at,
     html_url, steps_json, log_state /* none | live | stored | expired */, cpu_peak, cpu_avg, mem_peak_mb, debug, enriched_at)
job_metrics(job_id, t_ms, cpu_percent, memory_mb, PRIMARY KEY(job_id, t_ms))
cache_entries(id, repository, ref, key, version, size_bytes, path, finalized, created_at, last_used_at)
cache_stats(hour INTEGER /* unix hour */, repository, hits, misses, bytes_read, bytes_written, PRIMARY KEY(hour, repository))
server_stats(server_id, t_ms, cpu_percent, memory_percent, disk_percent, PRIMARY KEY(server_id, t_ms))
notification_channels(id, name, kind /* slack | discord | webhook */, url_enc BLOB, events_json, enabled, created_at, updated_at,
                      last_sent_at, last_error /* empty when the last delivery was successful */)
backups(id, created_at, path, size_bytes, s3_key, status, error)
audit_log(id, at, user_id, action, target, detail_json, ip)
```

Indexes: jobs on `queued_at`, `state`, `repository`, `pool_id`, and `server_id`. Sandboxes on `state`, and on `ip` for the sandboxes that are not `gone` or `failed` (`sandboxes_live_ip`). Audit log on `at`.

`store.DeleteServer` also removes the server ID from `server_ids` of each pool, in the same transaction. If a pool loses its last server, the store disables the pool, because an empty `server_ids` means all servers. A pool from before this rule can still hold the ID of a deleted server. `PATCH /api/pools/{id}` accepts such an ID if the pool has it already, and refuses a new unknown ID.

Settings keys and defaults. `store.Settings` is a typed struct. The table stores one JSON value for each key.

| Key | Default | Use |
| --- | --- | --- |
| `instance_name` | `runplane` | Sidebar title |
| `public_url` | empty | Browser URL. If empty, the API uses the request host. |
| `domain`, `acme_email` | empty | HTTPS with ACME on ports 443 and 80 |
| `mode` | from install | `container`, `vm`, or `dev` |
| `reserve_vcpu`, `reserve_memory_mb` | 1, 2048 | Host reserve |
| `cpu_overcommit` | 1.0 | |
| `min_free_disk_percent` | 15 | |
| `default_time_limit_minutes` | 360 | |
| `allow_public_repos` | false | VM mode only. Container mode always rejects public repositories. `PATCH /api/settings` refuses to set it to true, and refuses a `mode` change while it is true, if the `mode` setting is not `vm` or if a server reports a different mode. A server with no reported mode does not count. The scheduler also examines each runner (section 8.1). |
| `allow_fork_prs` | false | |
| `egress_allowlist` | empty array | Empty = all public internet |
| `bandwidth_mbps` | 0 | Bandwidth limit of each sandbox in VM mode (rate limiter), 0 = no limit |
| `dns_servers` | `["1.1.1.1","8.8.8.8"]` | |
| `registry_mirror` | true | |
| `actions_cache` | true | |
| `cache_max_gb` | 50 | The limit of the entries and the unfinished uploads together, at most a quarter of the disk of the cache folder. runplane removes the least recently used entries first, also to keep `min_free_disk_percent` (plus 5 points) free; below `min_free_disk_percent` it takes no upload. One sandbox may have 64 unfinished uploads. `max_bytes` of `GET /api/caches` is this limit (`cache.Server.Limit`). |
| `job_retention_days` | 90 | |
| `log_retention_days` | 30 | |
| `debug_hold_minutes` | 30 | |
| `auto_update` | false | The primary installs new releases each day. The agents then get the release of the primary (section 5). |
| `backup_enabled`, `backup_keep` | true, 7 | Daily |
| `backup_s3` | empty object | Endpoint, region, bucket, prefix, access key, secret key (sealed) |

## 7. GitHub (`internal/github`)

- Manifest flow: `POST /api/github/manifest` returns the GitHub URL and the manifest JSON. The browser posts a form to GitHub. The manifest has `redirect_url = <base>/api/github/callback`, `setup_url = <base>/api/github/setup`, `public: false`, no `hook_attributes`, and the permissions in section 1. runplane keeps a random `state` value for 1 hour.
- `GET /api/github/callback?code&state` calls `POST /app-manifests/{code}/conversions`. It seals the key and the secrets, stores the app, and sends the browser to `https://github.com/apps/<slug>/installations/new`.
- `GET /api/github/setup?installation_id&setup_action` stores the installation, syncs the repositories, and makes the scale sets. Then it sends the browser to `/setup/first-job` (or to `/settings/github` after the setup).
- Installation sync each 5 minutes: installations, repositories, suspended state.
- Scale sets: for an organization installation, one scale set for each pool in the runner group `Default` at `https://github.com/<org>`. For a personal installation, one scale set for each pool in each selected repository at `https://github.com/<owner>/<repo>`. The name and the label are the pool label.
- One listener (`scaleset/listener`) for each scale set. The scheduler gives the `maxCapacity` value. Messages go to the scheduler `Scale` method for the pool.
- Run check: before the scheduler acquires a job of an enabled pool, `CheckJob` reads the run (`GET /repos/{o}/{r}/actions/runs/{run_id}`), one time for each run. It gives the private flag, the fork flag, `event`, `head_branch`, and the number and base branch of the first pull request of the run.
- Enrichment: when a job starts or completes, read the run (`GET /repos/{o}/{r}/actions/runs/{run_id}`) and its jobs. Fill `github_job_id`, `run_number`, `head_branch`, `head_sha`, `commit_message`, `actor`, `event`, `pr_number`, `html_url`, and `steps_json`. For a `cancelled` job, also take another result from the conclusion of the GitHub job (section 8.4). While a job runs, `Steps` reads `GET /repos/{o}/{r}/actions/jobs/{job_id}` each 30 seconds for `steps_json`.
- Errors of the run check: a 404, a 403 that is not a rate limit, and a repository with no installation wrap `scheduler.ErrRunUnreadable`.
- Logs: after the job completes, download `GET /repos/{o}/{r}/actions/jobs/{job_id}/logs` to `/var/lib/runplane/logs/<job-id>.log`. While the job operates, the job log comes from the sandbox (`Driver.Logs`).
- Re-run: `POST /repos/{o}/{r}/actions/jobs/{job_id}/rerun`. For a debug re-run, the scheduler marks the next attempt of the same run and job name as debug.
- Runner releases: each hour, read `GET /repos/actions/runner/releases/latest`. Read the end dates with the runner deprecations API. Make a new bundle for each arch that has a server. Send `runner.version` events. Show a warning 14 days before an end date.

## 8. Scheduler (`internal/scheduler`)

### 8.1 Scale for each pool and scale set

For each listener message: acquire the wanted `JobAvailable` jobs, record the job messages in `jobs` (idempotent on `runner_request_id`), keep the statistics, then resize. The necessary sandbox count is `min(min_idle + TotalAssignedJobs, max_runners, capacity)`.

For each job of an enabled pool, the scheduler reads the run (section 7) before it acquires the job. It records the run data on the job row for the cache refs (section 5.2).

The scheduler rejects a job from a public repository if `allow_public_repos` is false, or if a runner that can get the job is not a VM: a live sandbox of the scale set, or an online server for the pool, whose driver is not in VM mode. The `mode` setting does not count, because each server runs its own mode and a mode change applies after a restart. It rejects a job from a fork pull request if `allow_fork_prs` is false. It rejects a job whose run runplane can never read (`scheduler.ErrRunUnreadable`, for example a repository without the GitHub App) with a reason that says so. The scheduler does not acquire a rejected job. The job row stays in `queued` with a `reject_reason` until the cleanup closes it (section 8.4). Another failure of the run read, such as a GitHub outage, skips only that job until GitHub offers it again; the other jobs of the message go on.

### 8.2 Placement

A sandbox goes to an `online` server with the same arch as the pool and in `pools.server_ids` (all servers if empty). With `allow_public_repos` on, only VM servers get sandboxes, because GitHub can give any job of a scale set to any of its runners. `Capacity.Fits` must be true, and the free disk must be more than `min_free_disk_percent`. Select the server with the most free memory. If no server has sufficient capacity, the scheduler does not start the sandbox. The `maxCapacity` value for each listener is the sandbox count of the pool plus the count of new sandboxes that fit.

### 8.3 Start and stop

- Start: call `GenerateJitRunnerConfig` with the runner name `<label>-<short id>`. Insert the `sandboxes` row with the state `starting`. Then call `Driver.Start`.
- Stop an idle sandbox: first call `RemoveRunner` on GitHub. If GitHub says that the runner is busy, keep the sandbox. Then call `Driver.Stop`.
- Debug sandbox: when it completes its job, call `Driver.Hold` for `debug_hold_minutes`.
- Running job: the scheduler reads the steps of the job from GitHub each 30 seconds (one request) until the job completes, and publishes `job.state` when they change.
- Replace a warm sandbox: before each resize, the scheduler compares each idle sandbox with a new sandbox of its pool on the same server. If the vCPU, memory, disk, image digest, arch, or runner version is different, if the sandbox waited for a job for the whole time limit, or if it is not a VM while `allow_public_repos` is on, the scheduler stops the idle sandbox. It removes the runner first. If GitHub says that the runner is busy, the sandbox stays. Then the resize starts new sandboxes. A busy sandbox stays until its job completes.

### 8.4 Cleanup loop (each minute and at start)

| Found | Action |
| --- | --- |
| A sandbox with no database row | Stop the sandbox. |
| A row (not `gone`) with no sandbox on an online server | Set the row to `gone`. |
| A runner of a runplane scale set with no sandbox | Remove the runner from GitHub. |
| A sandbox past its time limit: the limit after `busy_at`, or the limit plus 5 minutes after `created_at` with no job, or its debug hold | Stop the sandbox. |
| A `starting` or `idle` row on a server with no connection for more than 15 minutes | Remove the runner from GitHub and set the row to `gone`. A busy row ends with its job. |
| A job in `running` with a `gone` sandbox for more than 10 minutes and no completion message | Complete the job with the result that the runner reported, or else `cancelled`. GitHub reports a completion one time, so a later JobCompleted with another result, or the conclusion of the GitHub job that the finish step reads, replaces a `cancelled` result. |
| A completed job of the last 7 days with `log_state` `live`, for example after a restart during its finish step | Run the finish step again (20 jobs at a time). |
| A JobCompleted with the result `cancelled` for a job that never started | GitHub took the assignment back. A later offer of the job opens it again, and the scheduler acquires it again. |
| A job in `queued` for more than 1 hour (runplane did not have the capacity, or GitHub gave the job to a different runner) | Set the state to `completed` and the result to `cancelled`. If the job has no `reject_reason`, set it to "runplane did not run this job.". If GitHub offers or assigns this job later, the job goes back to `queued`. |

## 9. HTTP API (`internal/api`)

All paths start with `/api`. JSON field names are `snake_case`. Errors use the Huma RFC 9457 problem JSON. Authentication: the cookie `runplane_session` (HttpOnly, SameSite=Lax, Secure with HTTPS). After 10 failed logins in 15 minutes for an IP address or an email, the login returns 429. A login also sets the cookie `runplane_device` (HttpOnly, SameSite=Strict, path `/api/session`, 400 days). This cookie has an HMAC of the user and a random ID. A browser with a valid device cookie for the user has its own count, not the count of the email. Thus failed logins from elsewhere cannot lock out a browser that logged in before. Each mutating request must have the header `X-Runplane-CSRF: 1` (the frontend sends it). Roles: `admin` can do all actions. `member` can read, re-run jobs, stop sandboxes, and open debug shells.

| Method | Path | Auth | Action |
| --- | --- | --- | --- |
| GET | `/api/status` | none | `{needs_setup, version, mode, instance_name, github_connected, has_jobs, ssh_port}` |
| POST | `/api/setup` | none, setup code | `{code, name, email, password}` makes the first admin and a session |
| GET | `/api/setup/host` | none, setup code in `?code` | Host check for setup step 1 |
| GET, POST, DELETE | `/api/session` | | Read the current user, log in `{email, password}`, log out |
| PATCH | `/api/me` | user | `{name, current_password, new_password}` |
| GET | `/api/overview?range=24h\|7d\|30d` | user | Stats, running jobs, servers, chart buckets, recent jobs |
| GET | `/api/github` | user | App, installations, listener status for each scale set |
| POST | `/api/github/manifest` | admin | `{account_type: organization\|user, organization}` → `{action_url, manifest}` |
| GET | `/api/github/callback` | browser | Manifest code exchange |
| GET | `/api/github/setup` | browser | Installation callback |
| POST | `/api/github/sync` | admin | Sync installations now |
| DELETE | `/api/github` | admin | Disconnect: stop listeners, delete scale sets, remove the app |
| GET, POST | `/api/pools` | user, admin | List (with live counts), make |
| GET | `/api/pools/placement?arch&vcpu&memory_mb&server_ids` | user | Count the servers where a sandbox of this size can start |
| GET, PATCH, DELETE | `/api/pools/{id}` | user, admin | |
| GET | `/api/sandboxes?pool_id&server_id&state` | user | Runners |
| GET | `/api/sandboxes/{id}` | user | |
| POST | `/api/sandboxes/{id}/stop` | member | |
| GET | `/api/sandboxes/{id}/log` | user | Text. `?follow=1` streams. |
| GET | `/api/sandboxes/{id}/shell` | member | WebSocket to the debug shell |
| GET | `/api/jobs?status&repository&pool_id&server_id&q&range&cursor&limit` | user | Cursor pages, newest first. `status`: running, queued, succeeded, failed, cancelled. |
| GET | `/api/jobs/facets` | user | Repositories, pools, and servers for the filters |
| GET | `/api/jobs/{id}` | user | Job with steps, `expected_duration_ms`, and `size_advice` (section 9.2). Before runplane reads the GitHub job, `html_url` is the page of the run: `<GitHub URL>/<repo>/actions/runs/<run_id>`, with `/attempts/<n>` for an attempt after the first. |
| GET | `/api/jobs/{id}/log?step=N` | user | Text. For a live job, the sandbox log (section 9.1). |
| GET | `/api/jobs/{id}/metrics` | user | CPU and memory samples |
| POST | `/api/jobs/{id}/rerun` | member | `{debug: bool}`. 409 for a job that is not `completed`. |
| GET | `/api/servers` | user | List with live stats and `update_state` (section 5) |
| POST | `/api/servers/tokens` | admin | `{name, mode}` → `{token, token_id, command, expires_at}`. `mode` is `auto` (default), `container`, or `vm`. If it is not `auto`, `command` ends with ` --mode <mode>`. |
| GET | `/api/servers/tokens/{id}/status` | admin | Tell if an agent used the install token |
| POST | `/api/servers/ssh` | admin | `{name, host, port, user, mode}` installs over SSH with the runplane SSH key. `mode` is as for the tokens. If it is not `auto`, the flags of the script get `--mode <mode>`. |
| GET | `/api/servers/ssh-key` | admin | Public key for `authorized_keys` |
| GET, PATCH, DELETE | `/api/servers/{id}` | user, admin | PATCH `{name, status: online\|draining\|disabled}` |
| GET | `/api/servers/{id}/doctor` | user | Last doctor result of the server |
| POST | `/api/servers/{id}/doctor` | admin | Run the doctor checks on that server and keep the result |
| POST | `/api/servers/{id}/update` | admin | Start the update of a connected agent to the release of the primary (section 5). 409 if `update_state` is `current`, `updating`, or `none`. Returns the server with `update_state: updating`. |
| GET, POST | `/api/images` | user, admin | List with the build status for each server, add `{name, ref}` |
| PATCH, DELETE | `/api/images/{id}` | admin | Set the default, remove |
| POST | `/api/images/{id}/refresh` | admin | Pull again |
| GET | `/api/caches` | user | Use for each repository, hit rate, mirror use of all servers, and `max_bytes`, the limit that the cache server uses |
| GET | `/api/caches/entries?repository` | user | Entries |
| DELETE | `/api/caches?repository&id` | admin | Remove entries |
| GET | `/api/runner-versions` | user | Bundles, latest version, end dates |
| GET, PATCH | `/api/settings` | user, admin | |
| GET, POST | `/api/notifications` | admin | |
| PATCH, DELETE | `/api/notifications/{id}` | admin | |
| POST | `/api/notifications/{id}/test` | admin | |
| POST | `/api/notifications/test` | admin | `{kind, url}`: send a test message to a channel that is not saved |
| GET | `/api/users` | admin | Users and open invites |
| PATCH | `/api/users/{id}` | admin | `{role}` |
| POST | `/api/invites` | admin | `{email, role}` → `{url}` |
| DELETE | `/api/invites/{id}`, `/api/users/{id}` | admin | |
| POST | `/api/invites/{id}/link` | admin | Replace an open invite with a new link. The old link stops. |
| GET, POST | `/api/invites/{id}` | none | Read an invite, accept `{name, password}`. The `id` path value is the invite token. |
| GET | `/api/audit?cursor` | admin | |
| GET, POST | `/api/backups` | admin | List, make one now |
| GET | `/api/backups/{id}/download` | admin | |
| GET, POST | `/api/update` | admin | Read the update status, apply the update. If the release repository has no release, GET shows `available: false`. runplane reads the release one time in 10 minutes. |
| GET | `/api/events` | user | SSE |
| GET | `/install.sh` | none | Install script with the values of the primary filled in: repository, agent address, fingerprint, release version, and release signing key |

Sandbox modes in the API:

| Field | Values |
| --- | --- |
| `mode` of `GET /api/status` and `GET /api/settings` | `container`, `vm`, `dev`, or empty until the install sets it |
| `mode` of `GET /api/setup/host` (`HostCheck`) | `container`, `vm`, or `dev` |
| `mode` of `PATCH /api/settings` | `container`, `vm`, or `dev` |
| `mode` of the `Server` object | `container`, `vm`, `dev`, or empty until the server reports its facts |

SSE event names and data:

| Event | Data |
| --- | --- |
| `sandbox.state` | Sandbox API object |
| `job.state` | Job API object (list shape) |
| `pool.stats` | `{pool_id, idle, busy, starting, queued, assigned}` |
| `server.state` | Server API object |
| `host.stats` | `{server_id, cpu_percent, memory_percent, disk_percent, at}` each 10 seconds |
| `runner.version` | Runner version object |
| `image.state` | Image API object |
| `github.state` | GitHub status object, as `GET /api/github` |
| `backup.state` | Backup API object, for each change of a backup row: `running`, `done`, `uploading`, `uploaded`, `failed`. Only admins get this event. |

A heartbeat comment line (`: ping`) goes out each 15 seconds.

### 9.1 Job log and steps

The stored GitHub log of a completed job has a time on each line, and the steps have start times. Thus `?step=N` can give the lines of one step. The live log of a running job comes from the sandbox (the `diag` lines) and has no step marks. For a live job, the API sends the whole log for each `step` value. If `step` is more than 0, the response has the header `X-Runplane-Step-Filter: ignored`. In dev mode, CORS lets the Vite dev server read this header.

### 9.2 Size advice

`GET /api/jobs/{id}` has `size_advice {kind, label, reason}`. The advice compares the use of the last 10 completed runs of the same repository, workflow file, and job name with the pool of the job. Only runs with metrics count. The CPU use of a run is in vCPUs of its own sandbox, thus a run on a different size counts correctly. Each value is the 90th percentile of the runs (nearest rank).

| Kind | Rule | Label |
| --- | --- | --- |
| `larger` | The memory peak is more than 90% of the pool memory, or the CPU average is more than 85% of the pool vCPUs. | The smallest enabled pool with the same arch that is not smaller in vCPUs or memory, and that keeps 30% headroom on each resource that is short. |
| `smaller` | The CPU peak and the memory peak are less than 40% of the pool. | The smallest enabled pool with the same arch that is smaller, and that keeps 30% headroom on the CPU peak and the memory peak. |
| `none` | Fewer than 3 runs, the pool does not exist, or the use fits the pool. | Empty |

If no pool fits, `label` is empty and `reason` says so. "Smallest" means the fewest vCPUs, then the least memory, then the label in alphabetical order.

### 9.3 Audit log

Each change through the API or the SSH gateway adds one row to `audit_log`. The `action` is `<object>.<verb>`, and the `target` is the ID of the changed object. The `detail` has the names and the values for the Activity sentences (section 10). A detail never holds a secret. Only admins can read the audit log.

| Action | Detail |
| --- | --- |
| `pool.create`, `pool.delete` | `label` |
| `pool.update` | `label`, and `changes` with `[old, new]` for each changed field. `image` and `servers` hold names, not IDs. An empty `image` is the default image. If no field changes, runplane adds no row. |
| `settings.update` | `keys` with all changed keys, and `changes` with `[old, new]` for each key. The fields of `backup_s3` have their own keys, for example `backup_s3.bucket`. A change of the S3 secret key shows in `keys` only. |
| `server.update` | `name` and `status` after the change, and `changes` with `[old, new]` for `name` and `status`. If nothing changes, runplane adds no row. |
| `server.token`, `server.install` | `name` and `mode` of the request. `server.token` also has `expires_at`. `server.install` also has `host`, `port`, and `user`. |
| `job.rerun` | `debug`, `repository`, `workflow_file`, `job_name`, `run_number` |
| `sandbox.stop`, `sandbox.shell` | `runner_name`. For a sandbox with a job, also `job_id`, `repository`, `workflow_file`, `job_name`, `run_number`. |
| `sandbox.ssh` | `key`, `github_login` or `admin_key`. For a sandbox with a job, also `job_name` and `repository`. |
| `invite.create`, `invite.delete` | `email`, `role` |

The other actions have the names of their objects, for example `name`, `email`, or `ref`. `runplane dev-seed` writes its rows with the same actions and details.

## 10. Frontend (`web/`)

- TanStack Start in SPA mode, TanStack Router file routes, TanStack Query, and `openapi-fetch`. The generated types are in `web/src/api/schema.d.ts`. `pnpm gen:api` makes them from `go run ./cmd/runplane openapi`.
- Styling and components: `GUI.md`, `gui/foundations.md`, `gui/components/runplane/*.md`, and `gui/themes/runplane.md`. Base UI (`@base-ui/react`) primitives. Tailwind CSS v4 with the theme CSS variables. Icons: `lucide-react`. Fonts: Inter and JetBrains Mono from `@fontsource-variable` packages (self-hosted, no CDN).
- Light and dark mode come from the theme. The user menu has System, Light, and Dark.
- One `EventSource` on `/api/events` updates the TanStack Query cache.
- Routes:

| Route | Screen | Design |
| --- | --- | --- |
| `/setup` | Step 1 "Server": setup code, admin account, host check | Continue the setup design |
| `/setup/github` | Step 2 "Connect GitHub" | Setup page, "Setup · Connect GitHub" |
| `/setup/first-job` | Step 3 "Run your first job" | Setup page, "Setup · First job" |
| `/login` | Log in | Continue the setup design |
| `/invite/$token` | Accept an invite | Continue the setup design |
| `/` | Overview, with the empty state when there are no jobs | "Overview", "Overview — Empty" |
| `/jobs` | Jobs table with filters and pages | "Jobs" |
| `/jobs/$jobId` | Job detail: steps, CPU and memory, size advice, log, re-run, debug shell | "Job detail", "Job detail — size advice" |
| `/servers` | Servers and the "Add server" dialog | "Servers", "Servers · Add server" |
| `/servers/$serverId` | Server detail: doctor results, sandboxes, images | Continue the design |
| `/runners` | Pools (labels) with live sandboxes, make and change pools. `?label=<label>` gives the row of that label the selected fill. | Continue the design |
| `/cache` | Actions cache use for each repository, registry mirror | Continue the design |
| `/settings/general` | Instance name, URL, HTTPS, retention, updates, backups | Continue the design |
| `/settings/github` | Section links General, GitHub, Runners, Notifications, Members. Three sections: GitHub App, Installed on, Job access. | "Settings · GitHub" |
| `/settings/runners` | Host reserve, overcommit, time limit, images, runner versions, network | Continue the design |
| `/settings/notifications` | Channels | Continue the design |
| `/settings/members` | Users, invites, and the activity (audit log) | "Settings · Members", "Settings · Members — activity" |

Frontend behavior that the routes do not show:

- Size advice: if `size_advice.kind` is not `none`, Job detail shows one line under the CPU and Memory cards. The line has the `reason` and, if `label` is set, a link to `/runners?label=<label>`.
- Agent updates: the Servers table shows **Update available**, **Updating**, or **Update failed** after the name of the server. Server detail shows the same text after the version, and **Update failed** has the status detail in a tooltip. The row menu and the menu of Server detail have **Update agent** (`POST /api/servers/{id}/update`) when `update_state` is `available` or `failed`.
- A `server.state` event has the Server API object, with `update_state`, the live stats, and the sandbox counts. The frontend puts the object in the cached `GET /api/servers` and `GET /api/servers/{id}`. If the cached list does not have the server, for example a new server, the frontend loads the list again. It also loads `GET /api/overview` and `GET /api/pools` again, because they count the servers.
- Runner log: when a runner leaves the live list, the log panel reads `GET /api/sandboxes/{id}`. For a `failed` sandbox, the panel shows "Failed" and the `error` (section 4.2, step 6).
- Activity: Settings · Members shows `GET /api/audit` to admins in pages of 10, with Previous and Next. A member does not see the section, and the page does not request the audit log for a member. Each row has the time, the user, a sentence from `action` and `detail`, and the name of the changed object. An action that the frontend does not know shows as it is. After each successful change in the dashboard, the frontend loads the audit log again.
- Number settings: Escape shows the saved value again, and the field keeps the focus. Enter or a blur saves the value.
- Runner log: if the stream stops with an error after some lines, the error shows under the lines with **Try again**.
- Add server: the two tabs share one `Select` field "Sandbox mode", with the options "Automatic (VM with KVM, else container)", "Container (Sysbox)", and "VM (Firecracker)". The default is Automatic. In the "Install script" tab, the field is between the description and the command. In the "Connect over SSH" tab, it is the first row of the form. A change of the field sends a new token request with that mode. While the new token loads, the dialog shows "Making an install token". The text of the dialog says that the script installs the sandbox runtime and the runplane agent.
- Server detail: in VM mode, the converting state of the Images table shows "Converting · to a VM disk".
- Settings · Runners: "Bandwidth limit" applies in VM mode only. The field is disabled in container mode and dev mode.
- Settings · GitHub, Job access: in container mode, the switch for public repositories is off and disabled. Its text is "Container mode always refuses jobs from public repositories. Switch to VM mode to run them." In VM mode and dev mode, its text is "Anyone who opens a pull request could run code on your servers." Fork jobs never get the secrets of the repository.

## 11. Build and release

- `make web`: `pnpm -C web install && pnpm -C web build`. Then copy the SPA output folder to `internal/webui/dist`.
- `make init`: `CGO_ENABLED=0 go build -o bin/runplane-init-linux-<arch> ./init` for the target arch, then a gzip copy to `internal/initbin/bin/runplane-init`. The gzip copy makes the runplane binary approximately 4 MB smaller.
- `make build`: web, init, then `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X ..." -o bin/runplane ./cmd/runplane`.
- `make build` and the release builds use no build tags. Thus they have no `dev-seed` command and no demo data. `make dev` builds with the tag `devseed`, which adds `internal/devseed` and the `dev-seed` command (section 12).
- `internal/initbin` embeds the init binary for the same arch. It unpacks a gzip file, and it also accepts a plain binary. If the embedded file is a placeholder, runplane looks for `runplane-init` next to its executable.
- Release: `.github/workflows/release.yml` builds `linux/amd64` and `linux/arm64`, writes `SHA256SUMS`, signs it with ed25519 (`SHA256SUMS.sig`), and uploads the assets. `images.yml` publishes the base images to GHCR. `kernel.yml` builds the guest kernels.
- `make dist` writes the repository (`REPO`) and the public key (`UPDATE_PUBLIC_KEY`) into the defaults of the release copy of `install.sh`. If `RUNPLANE_PUBLIC_KEY` is set, install.sh downloads `SHA256SUMS.sig` and stops if the signature is not correct (OpenSSL 3). The primary serves install.sh with the key of its binary.
- `make dist` runs `make size-check` first. `size-check` stops with an error if `bin/runplane-linux-amd64` or `bin/runplane-linux-arm64` has 30,000,000 bytes or more.
- install.sh keeps the role of a server. A run without `--token` on a server with `agent.json` or `runplane-agent.service` updates the agent and keeps `agent.json`. A run with `--token` on a server with `runplane.service` stops.
- Self-update (`internal/update`): `runplane update`, `POST /api/update`, the auto update, and the agent update download `SHA256SUMS` and `SHA256SUMS.sig` of the release. They examine the signature and the SHA-256 of each file. If `/var/lib/runplane/kernel/vmlinux` exists (VM mode), the update also downloads `vmlinux-x86_64` or `vmlinux-arm64` and replaces the guest kernel before the binary. Each new file goes to a temporary file in the same folder, and a rename replaces the old file. A release without the guest kernel stops the update of a server that has one. VMs that operate keep the old kernel, because their chroots have a hard link to it.
- In VM mode, install.sh examines whether `ghcr.io/<repo>/runner-ubuntu-24.04:latest` is available without a login. If not, runplane builds the base image from the Dockerfile in its binary, with Docker. Thus install.sh installs Docker Engine (Ubuntu and Debian only), and the unit gets `Wants=docker.service`, because runplane connects to Docker at its start. The unit always has `After=docker.service` (section 4.4). Docker builds images only. The jobs operate in VMs.
- In VM mode, install.sh installs the Firecracker release `RUNPLANE_FIRECRACKER_VERSION` (default `v1.17.0`, the release of the e2e tests). It downloads the release file and its SHA-256 from GitHub, without the GitHub API. If the installed `firecracker` has that version and `jailer` exists, install.sh keeps them.
- In VM mode, install.sh gives the user `runplane-vm` the uids and gids 1879048192 to 1879064575 (16384 IDs, one for each TAP index of `10.214.0.0/16`) in `/etc/subuid` and `/etc/subgid` (section 4.3). The range starts after the container ranges of systemd. If the user has a range, install.sh keeps it. If a different range or a user or group has an ID in the range, install.sh stops. The uninstall removes the range.
- `--mode auto` gives VM mode with KVM, else container mode. A server keeps its mode from the dashboard settings (primary) or from `runplane.env` (agent).

## 12. Commands (`cmd/runplane`)

`runplane help` shows the commands. `runplane <command> -h` shows the flags. The flags are the same as the environment variables (section 3). `sudo` starts a command without the variables of `runplane.env`. Thus `doctor`, `update`, `reset-password`, `setup-code`, and `set-mode` read the `RUNPLANE_*` variables of `runplane.env` that the environment does not have.

| Command | Use |
| --- | --- |
| `serve` | Operate the primary: dashboard, API, scheduler, and the sandboxes of this server. |
| `agent` | Operate an agent. The agent reads `/etc/runplane/agent.json` and needs `RUNPLANE_MODE`. |
| `doctor` | Do the host checks of this server and show the results. On the primary, the mode and `min_free_disk_percent` come from the settings. If the `domain` setting is set, the port checks include ports 443 and 80. In container and dev mode, the checks include the sandbox internet check. In VM mode, only the running service can do that check (section 4.3), thus the command shows a warning for it. If a check fails, the exit code is 1. install.sh runs this command after the start of the service. |
| `update [-check]` | Install the latest signed release, and in VM mode its guest kernel (section 11). With `-check`, only show the latest release. |
| `reset-password <email>` | Set a new random password for the user, show it, and remove the sessions of the user. |
| `setup-code` | Show a new one-time setup code. |
| `set-mode container\|vm\|dev` | Write the sandbox mode to the settings of the primary. After the first start, the settings win over `RUNPLANE_MODE`. install.sh runs this command for `--mode` on a primary that has a database. The mode applies at the next start. In container mode, the command also sets `allow_public_repos` to false. |
| `dev-seed` | Fill an empty database with demo data. For development only: only a build with the tag `devseed` has this command (section 11). The demo data has no `mode` setting, and its notification channels use hosts under `.invalid`. |
| `openapi` | Write the OpenAPI spec as JSON. |
| `version` | Show the version. |

The sandbox internet check starts a short probe container on the `runplane` Docker network, with the runtime and the firewall rules of a sandbox. The probe fetches `https://api.github.com`. While the server has no runplane image, the check shows a warning, not a failure. In VM mode, the doctor of the running service uses the probe of section 4.3, which needs no image.

The check "Base image source" (`base_image`) reads the manifest of `ghcr.io/<repo>/runner-ubuntu-24.04:latest` for the arch of the server. If the read is successful, the server pulls the image. If not, and Docker answers, the server builds the image from its Dockerfile. If not, and Docker does not answer, the check shows a failure. In VM mode, the fix of the check tells the admin to run install.sh again, which installs Docker Engine.

`serve` and `agent` do not log the errors of a canceled context after the stop starts.
