//go:build devseed

package devseed

import (
	"strings"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

// The demo team is Kestrel. Names, numbers, and texts come from the Paper
// designs where the designs show them.

const (
	instanceName = "runplane"
	publicURL    = "https://ci.kestrel.dev"
	password     = "runplane-dev-password"

	appID            = 1048213
	appSlug          = "runplane-kestrel"
	orgLogin         = "kestrel-hq"
	orgAccountID     = 148213377
	orgInstallation  = 58213907
	userLogin        = "ada-okafor"
	userAccountID    = 9127734
	userInstallation = 58214410

	runnerVersion    = "2.340.0" // the latest runner release
	oldRunnerVersion = "2.339.1"
	agentVersion     = "v0.4.2"
)

// setupDate is the day Ada set up runplane: the design shows the GitHub App
// as "created Oct 2, 2026".
var setupDate = time.Date(2026, 10, 2, 9, 14, 0, 0, time.UTC)

// hourWeights is the relative push rate for each UTC hour of a weekday: busy
// from 09:00 to 17:00 and quiet at night, as in the "Jobs per hour" chart.
// The nightly schedules add the jobs at 03:00.
var hourWeights = [24]float64{
	0.07, 0.05, 0.06, 0.08, 0.05, 0.04, 0.08, 0.17, 0.37, 0.75, 0.97, 1,
	0.75, 0.92, 0.95, 0.83, 0.88, 0.78, 0.48, 0.32, 0.23, 0.17, 0.13, 0.10,
}

const (
	weekendFactor = 0.3
	pushes        = 295 // expected background pushes in 30 days, about 1,300 jobs
)

const (
	gib = 1 << 30
	tib = 1 << 40
)

type serverSpec struct {
	name, kind, status, detail string
	region                     string
	facts                      store.ServerFacts
	cpu, mem, disk             float64       // the latest host sample, in percent
	seen                       time.Duration // last heartbeat this long before now; 0 = never
	added                      time.Duration // added this long after the setup; an installing server minutes ago
}

var serverSpecs = []serverSpec{
	{
		name: "fsn1-ax102-a", kind: store.ServerLocal, status: store.ServerOnline, region: "fsn1",
		facts: vmHost("136.243.18.40", "Hetzner", "Ubuntu 24.04", "6.8.0-87-generic", 32, 128*1024, 19*tib/10),
		cpu:   52, mem: 38, disk: 34, seen: 3 * time.Second,
	},
	{
		name: "fsn1-ax52-a", kind: store.ServerAgent, status: store.ServerOnline, region: "fsn1",
		facts: vmHost("136.243.18.77", "Hetzner", "Ubuntu 24.04", "6.8.0-87-generic", 16, 64*1024, tib),
		cpu:   88, mem: 61, disk: 41, seen: 4 * time.Second, added: 2 * time.Hour,
	},
	{
		name: "fsn1-ax52-b", kind: store.ServerAgent, status: store.ServerOnline, region: "fsn1",
		facts: vmHost("136.243.19.12", "Hetzner", "Ubuntu 24.04", "6.8.0-87-generic", 16, 64*1024, tib),
		cpu:   31, mem: 22, disk: 38, seen: 6 * time.Second, added: 26 * time.Hour,
	},
	{
		name: "hel1-cax41", kind: store.ServerAgent, status: store.ServerOnline, region: "hel1",
		facts: containerHost("65.21.144.9", "Hetzner", "Ubuntu 24.04", "6.8.0-87-generic", "arm64", 16, 32*1024, 320*gib),
		cpu:   22, mem: 17, disk: 29, seen: 2 * time.Second, added: 27 * time.Hour,
	},
	{
		name: "gra-b2-30", kind: store.ServerAgent, status: store.ServerInstalling, detail: "pulling runner image", region: "gra",
		facts: store.ServerFacts{
			Address: "51.68.42.110", Provider: "OVH", OS: "Debian 12", Kernel: "6.1.0-40-cloud-amd64",
			Arch: "x64", Mode: sandbox.ModeContainer, VCPU: 8, MemoryMB: 30 * 1024, DiskBytes: 200 * gib,
			DiskKind: "SSD", DockerVersion: "28.5.1",
		},
	},
	{
		name: "ash-dedi-01", kind: store.ServerAgent, status: store.ServerUnreachable, region: "ash",
		facts: func() store.ServerFacts {
			f := vmHost("5.161.80.3", "Hetzner", "Ubuntu 22.04", "5.15.0-160-generic", 8, 32*1024, 512*gib)
			f.Version = "v0.4.1"
			return f
		}(),
		cpu: 18, mem: 24, disk: 52, seen: 2 * time.Hour, added: 30 * time.Hour,
	},
}

func vmHost(addr, provider, os, kernel string, vcpu, memMB int, disk int64) store.ServerFacts {
	return store.ServerFacts{
		Address: addr, Provider: provider, OS: os, Kernel: kernel, Arch: "x64", Mode: sandbox.ModeVM,
		VCPU: vcpu, MemoryMB: memMB, DiskBytes: disk, DiskKind: "NVMe", KVM: true,
		DockerVersion: "28.5.1", FirecrackerVersion: "1.13.1", Version: agentVersion,
	}
}

func containerHost(addr, provider, os, kernel, arch string, vcpu, memMB int, disk int64) store.ServerFacts {
	return store.ServerFacts{
		Address: addr, Provider: provider, OS: os, Kernel: kernel, Arch: arch, Mode: sandbox.ModeContainer,
		VCPU: vcpu, MemoryMB: memMB, DiskBytes: disk, DiskKind: "SSD",
		DockerVersion: "28.5.1", SysboxVersion: "0.6.7", Version: agentVersion,
	}
}

type poolSpec struct {
	name, label, arch string
	vcpu, memMB       int
	diskGB            int
	minIdle, max      int
}

var poolSpecs = []poolSpec{
	{"Small", "runplane-2cpu", "x64", 2, 8 * 1024, 40, 2, 24},
	{"Medium", "runplane-4cpu", "x64", 4, 16 * 1024, 60, 1, 16},
	{"Large", "runplane-8cpu", "x64", 8, 32 * 1024, 80, 1, 12},
	{"Extra large", "runplane-16cpu", "x64", 16, 64 * 1024, 120, 0, 4},
	{"ARM medium", "runplane-4cpu-arm", "arm64", 4, 8 * 1024, 60, 1, 8},
}

// orgRepos are the 12 repositories of kestrel-hq. userRepos are the 3 that
// ada-okafor selected.
var (
	orgRepos = []string{
		"api", "web", "mobile", "infra", "docs", "billing-worker",
		"design-system", "search", "cli", "terraform-modules", "status-page", ".github",
	}
	userRepos = []string{"blog", "dotfiles", "kestrel-bench"}
)

type repoSpec struct {
	name      string  // owner/name
	pushes    float64 // relative push rate
	prShare   float64 // share of pushes to pull requests
	actors    []string
	branches  []branch
	messages  []string
	workflows []*workflow
}

// personal reports if the repository belongs to a personal account.
func (r *repoSpec) personal() bool { return strings.HasPrefix(r.name, userLogin+"/") }

type branch struct {
	name  string
	pr    int
	actor string
}

type workflow struct {
	file, name string
	onPR       bool
	onPush     float64 // share of pushes to main that start the workflow
	nightly    bool    // also runs on a schedule at 03:00 UTC
	jobs       []*jobKind
}

type jobKind struct {
	name  string
	pool  string  // pool label
	secs  float64 // median run time of a succeeded job
	fail  float64 // share of failed runs
	cpu   float64 // CPU percent in the main step
	memMB int     // memory peak
	after string  // the job of the same run that must succeed first
	steps []step
}

type step struct {
	name  string
	secs  float64 // typical length; 0 marks the main step, which takes the rest of the job
	run   string  // the text after "##[group]Run "; empty for the runner's own steps
	cache bool    // the step restores an Actions cache
	out   []string
	fail  []string // output of the main step when it fails
}

// Placeholders in the step output: {repo} owner/name, {name} the repository
// name, {short} the short commit SHA, {message} the commit message, {arch},
// and {lock} the lockfile hash of the repository.

var (
	setUpJob    = step{name: "Set up job", secs: 2}
	completeJob = step{name: "Complete job", secs: 1}
	checkout    = step{name: "Checkout", secs: 3, run: "actions/checkout@v4", out: []string{
		"Syncing repository: {repo}",
		"Fetching the repository",
		"Determining the checkout info",
		"HEAD is now at {short} {message}",
	}}
	setupNode22 = setupNode("22", "22.21.1", "10.9.4")
	setupNode20 = setupNode("20", "20.19.5", "10.8.2")
	pnpmCache   = step{name: "Restore pnpm cache", secs: 4, run: "actions/cache@v4", cache: true, out: []string{
		"Cache hit for: pnpm-store-Linux-{arch}-{lock}",
		"Received 369098752 of 369098752 (100.0%), 104.2 MBs/sec",
		"Cache restored from key: pnpm-store-Linux-{arch}-{lock}",
	}}
	install = step{name: "Install dependencies", secs: 17, run: "pnpm install --frozen-lockfile", out: []string{
		"Lockfile is up to date, resolution step is skipped",
		"Packages: +1342",
		"Progress: resolved 1342, reused 1342, downloaded 0, added 1342, done",
		"Done in 16.8s using pnpm v10.18.3",
	}}
)

func setupNode(major, version, npm string) step {
	return step{name: "Set up Node " + major, secs: 1, run: "actions/setup-node@v4", out: []string{
		"Found in cache @ /opt/hostedtoolcache/node/" + version + "/{arch}",
		"Environment details",
		"  node: v" + version,
		"  npm: " + npm,
	}}
}

// nodeJob returns the steps of a Node job around its main step.
func nodeJob(node, main step) []step {
	return []step{setUpJob, checkout, node, pnpmCache, install, main, completeJob}
}

var (
	eslint = step{name: "Run ESLint", run: "pnpm lint", out: []string{
		"> {name}@0.0.0 lint /home/runner/work/{name}/{name}",
		"> eslint . --max-warnings 0",
		"",
	}, fail: []string{
		"> {name}@0.0.0 lint /home/runner/work/{name}/{name}",
		"> eslint . --max-warnings 0",
		"",
		"/home/runner/work/{name}/{name}/src/lib/format.ts",
		"  41:7  error  'session' is assigned a value but never used  @typescript-eslint/no-unused-vars",
		"",
		"✖ 1 problem (1 error, 0 warnings)",
	}}
	tsc = step{name: "Run tsc", run: "pnpm typecheck", out: []string{
		"> {name}@0.0.0 typecheck /home/runner/work/{name}/{name}",
		"> tsc --noEmit",
		"",
	}, fail: []string{
		"> {name}@0.0.0 typecheck /home/runner/work/{name}/{name}",
		"> tsc --noEmit",
		"",
		"src/lib/format.ts(41,7): error TS2322: Type 'string | undefined' is not assignable to type 'string'.",
	}}
	vitest = step{name: "Run tests", run: "pnpm test", out: []string{
		"> {name}@0.0.0 test /home/runner/work/{name}/{name}",
		"> vitest run",
		"",
		" RUN  v3.2.4 /home/runner/work/{name}/{name}",
		"",
		" Test Files  214 passed (214)",
		"      Tests  1873 passed (1873)",
	}, fail: []string{
		"> {name}@0.0.0 test /home/runner/work/{name}/{name}",
		"> vitest run",
		"",
		" FAIL  src/billing/invoice.test.ts > invoice > splits seats across billing periods",
		"AssertionError: expected 4200 to be 4199 // Object.is equality",
		"",
		" Test Files  1 failed | 213 passed (214)",
		"      Tests  1 failed | 1872 passed (1873)",
	}}
	integration = []step{
		setUpJob,
		{name: "Initialize containers", secs: 9, out: []string{
			"Pulling image postgres:16",
			"Starting postgres service container",
			"Waiting for all services to be ready",
			"postgres service is healthy.",
		}},
		checkout, setupNode22, pnpmCache, install,
		{name: "Run migrations", secs: 5, run: "pnpm db:migrate", out: []string{"Applied 87 migrations in 3.2s"}},
		{name: "Run integration tests", run: "pnpm test:integration", out: []string{
			" Test Files  41 passed (41)",
			"      Tests  388 passed (388)",
		}, fail: []string{
			" FAIL  test/billing/upgrade.test.ts > upgrade > keeps the trial end date",
			`error: duplicate key value violates unique constraint "subscriptions_account_id_key"`,
			"",
			" Test Files  1 failed | 40 passed (41)",
			"      Tests  1 failed | 387 passed (388)",
		}},
		{name: "Stop containers", secs: 2, out: []string{"Stop and remove container: postgres"}},
		completeJob,
	}
)

// webkitFailure is the Playwright log of the "Job detail" design.
var webkitFailure = []string{
	"> web@0.0.0 test:e2e /home/runner/work/web/web",
	"> playwright test --project=webkit",
	"",
	"Running 148 tests using 8 workers",
	"",
	"  ✓  checkout/cart.spec.ts:14:3 › adds item to cart (2.1s)",
	"  ✓  checkout/cart.spec.ts:38:3 › applies promo code (1.8s)",
	"  ✓  auth/login.spec.ts:9:3 › signs in with email (1.2s)",
	"  ✘  checkout/redirect.spec.ts:22:3 › returns to order page after payment (30.0s)",
	"",
	"  1) [webkit] › checkout/redirect.spec.ts:22:3 › returns to order page after payment",
	"",
	`     Error: Timed out 30000ms waiting for expect(page).toHaveURL(/\/orders\/\w+/)`,
	"",
	`     Expected pattern: /\/orders\/\w+/`,
	`     Received string:  "http://localhost:3000/checkout/return?session_id=cs_test_a1"`,
	"",
	"       29 |   await page.getByRole('button', { name: 'Pay now' }).click();",
	`     > 30 |   await expect(page).toHaveURL(/\/orders\/\w+/);`,
	"",
	"  1 failed",
	"  147 passed (5.4m)",
}

var chromiumFailure = []string{
	"> web@0.0.0 test:e2e /home/runner/work/web/web",
	"> playwright test --project=chromium",
	"",
	"Running 148 tests using 8 workers",
	"",
	"  ✘  account/orders.spec.ts:31:3 › lists past orders (12.4s)",
	"",
	"  1) [chromium] › account/orders.spec.ts:31:3 › lists past orders",
	"",
	"     Error: expect(locator).toHaveCount(expected) failed",
	"",
	"  1 failed",
	"  147 passed (4.7m)",
}

func e2eJob(browser, title, version string, secs, fail float64, failure []string) *jobKind {
	return &jobKind{
		name: "e2e · " + browser, pool: "runplane-8cpu", secs: secs, fail: fail, cpu: 74, memMB: 11200,
		steps: []step{
			setUpJob, checkout, setupNode22, pnpmCache,
			{name: "Install dependencies", secs: 18, run: install.run, out: install.out},
			{name: "Install Playwright browsers", secs: 41, run: "pnpm exec playwright install --with-deps " + browser, out: []string{
				"Downloading " + title + " " + version + " from https://cdn.playwright.dev/dbazure/download/playwright",
				title + " " + version + " downloaded to /home/runner/.cache/ms-playwright",
			}},
			{name: "Run Playwright (" + browser + ")", run: "pnpm test:e2e --project=" + browser, out: []string{
				"> web@0.0.0 test:e2e /home/runner/work/web/web",
				"> playwright test --project=" + browser,
				"",
				"Running 148 tests using 8 workers",
				"",
				"  148 passed (4.9m)",
			}, fail: failure},
			{name: "Upload report", secs: 3, run: "actions/upload-artifact@v4", out: []string{
				"With the provided path, there will be 214 files uploaded",
				"Artifact playwright-report-" + browser + " has been successfully uploaded! Final size is 3412871 bytes.",
			}},
			completeJob,
		},
	}
}

func buildImage(secs float64) *jobKind {
	return &jobKind{name: "build-image", pool: "runplane-4cpu", secs: secs, fail: 0.02, cpu: 66, memMB: 6100, steps: []step{
		setUpJob, checkout,
		{name: "Set up Docker Buildx", secs: 4, run: "docker/setup-buildx-action@v3", out: []string{
			"Docker info",
			"Creating a new builder instance",
			"Booting builder",
		}},
		{name: "Log in to ghcr.io", secs: 1, run: "docker/login-action@v3", out: []string{
			"Logging into ghcr.io...",
			"Login Succeeded!",
		}},
		{name: "Build and push", run: "docker/build-push-action@v6", out: []string{
			"#1 [internal] load build definition from Dockerfile",
			"#12 [build 6/6] RUN pnpm build",
			"#14 exporting to image",
			"#14 pushing layers done",
			"#14 pushing manifest for ghcr.io/{repo}:{short} done",
		}, fail: []string{
			"#12 [build 6/6] RUN pnpm build",
			`#12 ERROR: process "/bin/sh -c pnpm build" did not complete successfully: exit code: 1`,
			`ERROR: failed to solve: process "/bin/sh -c pnpm build" did not complete successfully: exit code: 1`,
		}},
		completeJob,
	}}
}

func deployStaging(secs float64) *jobKind {
	return &jobKind{name: "deploy-staging", pool: "runplane-2cpu", secs: secs, fail: 0.02, cpu: 14, memMB: 900, after: "build-image", steps: []step{
		setUpJob, checkout,
		{name: "Deploy to staging", run: "./scripts/deploy.sh staging", out: []string{
			"Deploying ghcr.io/{repo}:{short} to staging",
			"Rolling update: 3/3 replicas ready",
			"Deployed {short} to https://staging.kestrel.dev",
		}, fail: []string{
			"Deploying ghcr.io/{repo}:{short} to staging",
			"Error: rollout status timed out after 120s",
		}},
		completeJob,
	}}
}

var repoSpecs = []*repoSpec{
	{
		name: "kestrel-hq/api", pushes: 3, prShare: 0.6,
		actors: []string{"ada-okafor", "jonas-b", "priya-s", "tomasz-w"},
		branches: []branch{
			{"feat/billing-v2", 1031, "jonas-b"},
			{"fix/rate-limit-headers", 1027, "priya-s"},
			{"feat/audit-export", 1022, "tomasz-w"},
			{"chore/pg-8.16", 1019, "ada-okafor"},
		},
		messages: []string{
			"Return Retry-After on rate limits", "Prorate seat changes mid-cycle", "Add audit log export endpoint",
			"Bump pg to 8.16.3", "Cache plan lookups per request", "Fix tax rounding for EU invoices",
			"Log webhook signature failures", "Drop legacy v1 billing routes",
		},
		workflows: []*workflow{
			{file: "ci.yml", name: "CI", onPR: true, onPush: 1, jobs: []*jobKind{
				{name: "lint", pool: "runplane-2cpu", secs: 42, fail: 0.02, cpu: 71, memMB: 1900, steps: nodeJob(setupNode22, eslint)},
				{name: "typecheck", pool: "runplane-4cpu", secs: 70, fail: 0.02, cpu: 86, memMB: 3400, steps: nodeJob(setupNode22, tsc)},
				{name: "test (node 20)", pool: "runplane-8cpu", secs: 165, fail: 0.04, cpu: 78, memMB: 5600, steps: nodeJob(setupNode20, vitest)},
				{name: "test (node 22)", pool: "runplane-8cpu", secs: 192, fail: 0.04, cpu: 79, memMB: 5800, steps: nodeJob(setupNode22, vitest)},
				{name: "integration (postgres 16)", pool: "runplane-16cpu", secs: 220, fail: 0.04, cpu: 58, memMB: 9400, steps: integration},
			}},
			{file: "deploy.yml", name: "Deploy", onPush: 1, jobs: []*jobKind{buildImage(111), deployStaging(55)}},
		},
	},
	{
		name: "kestrel-hq/web", pushes: 3.5, prShare: 0.65,
		actors: []string{"mira-k", "priya-s", "lee-h", "ada-okafor"},
		branches: []branch{
			{"fix/checkout-redirect", 412, "mira-k"},
			{"feat/order-history", 409, "priya-s"},
			{"feat/dark-mode", 401, "lee-h"},
			{"renovate/vite-7", 415, "renovate[bot]"},
		},
		messages: []string{
			"Show order history on the account page", "Keep session_id on checkout return", "Add dark mode tokens",
			"Lazy-load the pricing table", "Fix focus ring on the plan picker", "Update vite to v7.1.12",
			"Prefetch product pages on hover", "Fix redirect loop after Stripe checkout",
		},
		workflows: []*workflow{
			{file: "ci.yml", name: "CI", onPR: true, onPush: 1, jobs: []*jobKind{
				{name: "lint", pool: "runplane-2cpu", secs: 33, fail: 0.02, cpu: 69, memMB: 1700, steps: nodeJob(setupNode22, eslint)},
				{name: "typecheck", pool: "runplane-4cpu", secs: 65, fail: 0.02, cpu: 84, memMB: 3100, steps: nodeJob(setupNode22, tsc)},
			}},
			{file: "e2e.yml", name: "E2E", onPR: true, onPush: 1, nightly: true, jobs: []*jobKind{
				e2eJob("chromium", "Chromium", "141.0.7390.37", 300, 0.05, chromiumFailure),
				e2eJob("webkit", "WebKit", "26.0", 400, 0.08, webkitFailure),
			}},
			{file: "deploy.yml", name: "Deploy", onPush: 1, jobs: []*jobKind{buildImage(140), deployStaging(50)}},
		},
	},
	{
		name: "kestrel-hq/mobile", pushes: 1.2, prShare: 0.6,
		actors: []string{"tomasz-w", "mira-k", "priya-s"},
		branches: []branch{
			{"renovate/expo-52", 233, "renovate[bot]"},
			{"feat/push-receipts", 229, "tomasz-w"},
			{"fix/android-deeplinks", 231, "mira-k"},
		},
		messages: []string{
			"chore(deps): update expo to v52", "Show push receipts in the inbox", "Fix Android deep links to orders",
			"Release 3.14.0", "Cache product images on device",
		},
		workflows: []*workflow{
			{file: "ci.yml", name: "CI", onPR: true, onPush: 1, jobs: []*jobKind{
				{name: "lint", pool: "runplane-4cpu-arm", secs: 38, fail: 0.02, cpu: 64, memMB: 1800, steps: nodeJob(setupNode22, eslint)},
				{name: "typecheck", pool: "runplane-4cpu-arm", secs: 75, fail: 0.02, cpu: 82, memMB: 3000, steps: nodeJob(setupNode22, tsc)},
			}},
			{file: "release.yml", name: "Release", onPush: 0.15, jobs: []*jobKind{
				{name: "android-build", pool: "runplane-8cpu", secs: 451, fail: 0.05, cpu: 88, memMB: 21500, steps: []step{
					setUpJob, checkout,
					{name: "Set up JDK 17", secs: 3, run: "actions/setup-java@v4", out: []string{
						"Java configuration:",
						"  Distribution: temurin",
						"  Version: 17.0.16+8",
					}},
					setupNode22,
					{name: "Restore Gradle cache", secs: 6, run: "actions/cache@v4", cache: true, out: []string{
						"Cache restored from key: gradle-Linux-{lock}",
					}},
					{name: "Install dependencies", secs: 25, run: install.run, out: install.out},
					{name: "Build release AAB", run: "./gradlew bundleRelease", out: []string{
						"> Task :app:bundleRelease",
						"",
						"BUILD SUCCESSFUL in 6m 41s",
						"1247 actionable tasks: 312 executed, 935 up-to-date",
					}, fail: []string{
						"> Task :app:mergeReleaseResources FAILED",
						"",
						"FAILURE: Build failed with an exception.",
						"",
						"BUILD FAILED in 4m 12s",
					}},
					{name: "Upload artifact", secs: 8, run: "actions/upload-artifact@v4", out: []string{
						"Artifact app-release has been successfully uploaded! Final size is 50438211 bytes.",
					}},
					completeJob,
				}},
			}},
		},
	},
	{
		name: "kestrel-hq/infra", pushes: 0.6, prShare: 0.7,
		actors: []string{"lee-h", "ada-okafor"},
		branches: []branch{
			{"feat/hel1-pool", 96, "lee-h"},
			{"chore/terraform-1.13", 94, "ada-okafor"},
		},
		messages: []string{"Add hel1 ARM runner pool", "Move DNS to the new zone", "Pin hcloud provider to 1.54", "Open port 8485 for agents"},
		workflows: []*workflow{
			{file: "plan.yml", name: "Terraform", onPR: true, onPush: 1, nightly: true, jobs: []*jobKind{
				{name: "terraform plan", pool: "runplane-2cpu", secs: 52, fail: 0.03, cpu: 22, memMB: 700, steps: []step{
					setUpJob, checkout,
					{name: "Set up Terraform", secs: 2, run: "hashicorp/setup-terraform@v3", out: []string{"Terraform v1.13.4"}},
					{name: "terraform init", secs: 8, run: "terraform init -input=false", out: []string{
						"Initializing the backend...",
						"Initializing provider plugins...",
						"Terraform has been successfully initialized!",
					}},
					{name: "terraform plan", run: "terraform plan -input=false -no-color", out: []string{
						`hcloud_server.runner["hel1-cax41"]: Refreshing state... [id=58122019]`,
						"",
						"Plan: 2 to add, 1 to change, 0 to destroy.",
					}, fail: []string{
						"Error: Invalid value for variable",
						"",
						"  on variables.tf line 14:",
						`  14: variable "server_type" {`,
						"",
						"server_type must be one of cax21, cax31, cax41.",
					}},
					completeJob,
				}},
			}},
		},
	},
	{
		name: "kestrel-hq/docs", pushes: 0.5, prShare: 0.3,
		actors:   []string{"lee-h", "priya-s"},
		branches: []branch{{"docs/runner-labels", 138, "lee-h"}},
		messages: []string{"Document runner labels", "Add billing FAQ", "Fix broken links in the API guide"},
		workflows: []*workflow{
			{file: "docs.yml", name: "Docs", onPR: true, onPush: 1, jobs: []*jobKind{
				{name: "build", pool: "runplane-2cpu", secs: 80, fail: 0.02, cpu: 55, memMB: 2400, steps: []step{
					setUpJob, checkout, setupNode22,
					{name: "Install dependencies", secs: 12, run: install.run, out: install.out},
					{name: "Build site", run: "pnpm build", out: []string{
						"> docs@0.0.0 build /home/runner/work/docs/docs",
						"> astro build",
						"",
						"[build] 214 page(s) built in 38.2s",
						"[build] Complete!",
					}, fail: []string{
						"> docs@0.0.0 build /home/runner/work/docs/docs",
						"> astro build",
						"",
						`[build] Could not resolve "../components/RunnerTable.astro" from "src/pages/runners.mdx"`,
					}},
					completeJob,
				}},
			}},
		},
	},
	{
		name: "kestrel-hq/billing-worker", pushes: 0.6, prShare: 0.5,
		actors:   []string{"tomasz-w", "jonas-b"},
		branches: []branch{{"fix/stripe-webhook-retry", 57, "tomasz-w"}},
		messages: []string{"Retry Stripe webhooks with backoff", "Batch ledger writes", "Bump Go to 1.25.3"},
		workflows: []*workflow{
			{file: "ci.yml", name: "CI", onPR: true, onPush: 1, jobs: []*jobKind{
				{name: "test", pool: "runplane-2cpu", secs: 95, fail: 0.03, cpu: 82, memMB: 3100, steps: []step{
					setUpJob, checkout,
					{name: "Set up Go", secs: 2, run: "actions/setup-go@v5", cache: true, out: []string{
						"Found in cache @ /opt/hostedtoolcache/go/1.25.3/x64",
						"Cache restored from key: setup-go-Linux-x64-ubuntu24-go-1.25.3-{lock}",
					}},
					{name: "Run tests", run: "go test -race ./...", out: []string{
						"ok  \tgithub.com/kestrel-hq/billing-worker/internal/stripe\t4.812s",
						"ok  \tgithub.com/kestrel-hq/billing-worker/internal/ledger\t9.104s",
						"ok  \tgithub.com/kestrel-hq/billing-worker/cmd/worker\t1.337s",
					}, fail: []string{
						"--- FAIL: TestRetryBackoff (0.31s)",
						"    retry_test.go:42: got 3 attempts, want 4",
						"FAIL\tgithub.com/kestrel-hq/billing-worker/internal/stripe\t5.022s",
						"FAIL",
					}},
					completeJob,
				}},
			}},
		},
	},
	{
		name: "ada-okafor/blog", pushes: 0.1,
		actors:   []string{"ada-okafor"},
		messages: []string{"Post: one binary for CI runners", "Fix typo on the about page"},
		workflows: []*workflow{
			{file: "build.yml", name: "Build", onPush: 1, jobs: []*jobKind{
				{name: "build", pool: "runplane-2cpu", secs: 40, fail: 0.02, cpu: 38, memMB: 600, steps: []step{
					setUpJob, checkout,
					{name: "Set up Hugo", secs: 2, run: "peaceiris/actions-hugo@v3", out: []string{"Hugo v0.152.2"}},
					{name: "Build", run: "hugo --minify", out: []string{"Total in 1834 ms"}, fail: []string{
						`Error: error building site: "content/posts/runners.md:12:1": unclosed shortcode`,
					}},
					completeJob,
				}},
			}},
		},
	},
}

// fixtureRun is a workflow run that a design shows.
type fixtureRun struct {
	repo, file           string
	number               int64
	branch, sha, message string
	actor                string
	pr                   int
	jobs                 []fixtureJob
}

type fixtureJob struct {
	name    string
	server  string        // "" = still queued
	ago     time.Duration // started this long before now; a queued job was queued this long before now
	queue   time.Duration
	dur     time.Duration // 0 = still running
	result  string
	steps   []int          // step lengths in seconds; nil = generated
	metrics *designMetrics // nil = generated
}

// fixtureRuns are the jobs of the last 40 minutes in the "Overview" and
// "Jobs" designs: 7 running jobs, 1 queued job, and the recent jobs.
var fixtureRuns = []fixtureRun{
	{repo: "kestrel-hq/api", file: "ci.yml", number: 4182, branch: "feat/billing-v2", sha: "9f3c2e1",
		message: "Prorate seat changes mid-cycle", actor: "jonas-b", pr: 1031, jobs: []fixtureJob{
			{name: "test (node 22)", server: "fsn1-ax52-a", ago: 102 * time.Second, queue: 2 * time.Second},
			{name: "typecheck", server: "fsn1-ax102-a", ago: 100 * time.Second, queue: time.Second},
			{name: "integration (postgres 16)", ago: 41 * time.Second},
		}},
	{repo: "kestrel-hq/web", file: "e2e.yml", number: 2291, branch: "fix/checkout-redirect", sha: "a71d0b4",
		message: "Keep session_id on checkout return", actor: "mira-k", pr: 412, jobs: []fixtureJob{
			{name: "e2e · chromium", server: "fsn1-ax52-a", ago: 185 * time.Second, queue: 3 * time.Second},
		}},
	{repo: "kestrel-hq/web", file: "deploy.yml", number: 918, branch: "main", sha: "5be8c90",
		message: "Show order history on the account page", actor: "priya-s", jobs: []fixtureJob{
			{name: "build-image", server: "fsn1-ax52-b", ago: 48 * time.Second, queue: time.Second},
		}},
	{repo: "kestrel-hq/mobile", file: "ci.yml", number: 1205, branch: "renovate/expo-52", sha: "4c19e0b",
		message: "chore(deps): update expo to v52.0.11", actor: "renovate[bot]", pr: 233, jobs: []fixtureJob{
			{name: "lint", server: "hel1-cax41", ago: 12 * time.Second, queue: 2 * time.Second},
		}},
	{repo: "kestrel-hq/docs", file: "docs.yml", number: 611, branch: "main", sha: "d42a7f3",
		message: "Document runner labels", actor: "lee-h", jobs: []fixtureJob{
			{name: "build", server: "fsn1-ax102-a", ago: 33 * time.Second, queue: 2 * time.Second},
		}},
	{repo: "kestrel-hq/billing-worker", file: "ci.yml", number: 842, branch: "fix/stripe-webhook-retry", sha: "8e1b5c2",
		message: "Retry Stripe webhooks with backoff", actor: "tomasz-w", pr: 57, jobs: []fixtureJob{
			{name: "test", server: "fsn1-ax102-a", ago: 58 * time.Second, queue: 3 * time.Second},
		}},
	{repo: "kestrel-hq/api", file: "ci.yml", number: 4181, branch: "main", sha: "1d0e7aa",
		message: "Return Retry-After on rate limits", actor: "ada-okafor", jobs: []fixtureJob{
			{name: "test (node 22)", server: "fsn1-ax102-a", ago: 5 * time.Minute, queue: 2 * time.Second, dur: 192 * time.Second, result: store.ResultSucceeded},
		}},
	{repo: "kestrel-hq/web", file: "e2e.yml", number: 2290, branch: "fix/checkout-redirect", sha: "c4e8f12",
		message: "Fix redirect loop after Stripe checkout", actor: "mira-k", pr: 412, jobs: []fixtureJob{
			{name: "e2e · webkit", server: "fsn1-ax52-a", ago: 11 * time.Minute, queue: 4 * time.Second, dur: 408 * time.Second,
				result: store.ResultFailed, steps: []int{2, 3, 1, 4, 18, 41, 332, 0, 1},
				metrics: &designMetrics{cpuPeak: 98, cpuAvg: 64, memMB: 11674}},
		}},
	{repo: "kestrel-hq/web", file: "ci.yml", number: 5520, branch: "fix/checkout-redirect", sha: "c4e8f12",
		message: "Fix redirect loop after Stripe checkout", actor: "mira-k", pr: 412, jobs: []fixtureJob{
			{name: "typecheck", server: "fsn1-ax102-a", ago: 10*time.Minute + 58*time.Second, queue: time.Second, dur: 65 * time.Second, result: store.ResultSucceeded},
		}},
	{repo: "kestrel-hq/api", file: "deploy.yml", number: 917, branch: "main", sha: "1d0e7aa",
		message: "Return Retry-After on rate limits", actor: "ada-okafor", jobs: []fixtureJob{
			{name: "build-image", server: "fsn1-ax52-b", ago: 14 * time.Minute, queue: 2 * time.Second, dur: 111 * time.Second, result: store.ResultSucceeded},
			{name: "deploy-staging", server: "fsn1-ax52-b", ago: 13*time.Minute + 50*time.Second, queue: time.Second, dur: 14 * time.Second, result: store.ResultCancelled},
		}},
	{repo: "kestrel-hq/mobile", file: "ci.yml", number: 1204, branch: "renovate/expo-52", sha: "77b2a9d",
		message: "chore(deps): update expo to v52", actor: "renovate[bot]", pr: 233, jobs: []fixtureJob{
			{name: "lint", server: "hel1-cax41", ago: 16 * time.Minute, queue: 3 * time.Second, dur: 38 * time.Second, result: store.ResultSucceeded},
		}},
	{repo: "kestrel-hq/infra", file: "plan.yml", number: 388, branch: "feat/hel1-pool", sha: "e09c3f5",
		message: "Add hel1 ARM runner pool", actor: "lee-h", pr: 96, jobs: []fixtureJob{
			{name: "terraform plan", server: "fsn1-ax102-a", ago: 23 * time.Minute, queue: time.Second, dur: 52 * time.Second, result: store.ResultSucceeded},
		}},
	{repo: "kestrel-hq/api", file: "ci.yml", number: 4180, branch: "feat/billing-v2", sha: "3a6d118",
		message: "Split invoice lines per seat", actor: "jonas-b", pr: 1031, jobs: []fixtureJob{
			{name: "test (node 20)", server: "fsn1-ax52-a", ago: 26 * time.Minute, queue: 2 * time.Second, dur: 160 * time.Second, result: store.ResultFailed},
		}},
	{repo: "kestrel-hq/mobile", file: "release.yml", number: 77, branch: "main", sha: "b81f04c",
		message: "Release 3.14.0", actor: "priya-s", jobs: []fixtureJob{
			{name: "android-build", server: "fsn1-ax102-a", ago: 31 * time.Minute, queue: 2 * time.Second, dur: 451 * time.Second, result: store.ResultSucceeded},
		}},
}

// cacheSpecs are the Actions cache entries. Each key ends with a hash of the
// repository, ref, and prefix.
var cacheSpecs = []struct {
	repo, ref, prefix string
	mb                int64
	age               time.Duration // created this long before now
}{
	{"kestrel-hq/api", "refs/heads/main", "pnpm-store-Linux-x64-", 352, 9 * 24 * time.Hour},
	{"kestrel-hq/api", "refs/pull/1031/merge", "pnpm-store-Linux-x64-", 355, 2 * 24 * time.Hour},
	{"kestrel-hq/api", "refs/pull/1027/merge", "pnpm-store-Linux-x64-", 351, 5 * 24 * time.Hour},
	{"kestrel-hq/web", "refs/heads/main", "pnpm-store-Linux-x64-", 486, 6 * 24 * time.Hour},
	{"kestrel-hq/web", "refs/pull/412/merge", "pnpm-store-Linux-x64-", 489, 26 * time.Hour},
	{"kestrel-hq/web", "refs/pull/415/merge", "pnpm-store-Linux-x64-", 471, 3 * 24 * time.Hour},
	{"kestrel-hq/web", "refs/pull/409/merge", "pnpm-store-Linux-x64-", 488, 8 * 24 * time.Hour},
	{"kestrel-hq/mobile", "refs/heads/main", "pnpm-store-Linux-arm64-", 402, 12 * 24 * time.Hour},
	{"kestrel-hq/mobile", "refs/heads/main", "pnpm-store-Linux-x64-", 398, 12 * 24 * time.Hour},
	{"kestrel-hq/mobile", "refs/heads/main", "gradle-Linux-", 1340, 4 * 24 * time.Hour},
	{"kestrel-hq/mobile", "refs/pull/233/merge", "pnpm-store-Linux-arm64-", 417, 20 * time.Hour},
	{"kestrel-hq/docs", "refs/heads/main", "pnpm-store-Linux-x64-", 128, 15 * 24 * time.Hour},
	{"kestrel-hq/billing-worker", "refs/heads/main", "setup-go-Linux-x64-ubuntu24-go-1.25.3-", 241, 11 * 24 * time.Hour},
	{"kestrel-hq/billing-worker", "refs/pull/57/merge", "setup-go-Linux-x64-ubuntu24-go-1.25.3-", 243, 30 * time.Hour},
}
