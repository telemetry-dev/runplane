package main

import (
	"context"
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/runplane/runplane"
	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/fleet"
	"github.com/runplane/runplane/internal/initbin"
	"github.com/runplane/runplane/internal/sandbox/firecracker"
	"github.com/runplane/runplane/internal/sandbox/guest"
	"github.com/runplane/runplane/internal/sandbox/netrules"
	"github.com/runplane/runplane/internal/sandbox/sysbox"
	"github.com/runplane/runplane/internal/update"
	"github.com/runplane/runplane/internal/version"
)

// TestInstallFilesAgreeWithGo compares the copies of Go values in install.sh,
// the Makefile, the workflows, and kernel/build.sh with the Go values. Each
// error names the files that must change together.
func TestInstallFilesAgreeWithGo(t *testing.T) {
	script := string(runplane.InstallScript)
	def := config.Default()
	roles := []struct {
		name, flags, unit string
		run               func(context.Context, []string) error
	}{
		{"primary", "", primaryUnit, serve},
		{"agent", "--token rp_srv_TEST --primary 203.0.113.7:8485 --fingerprint sha256:" + strings.Repeat("0", 64), agentUnit, runAgent},
	}

	t.Run("paths and names", func(t *testing.T) {
		vars := installVars(t, "CONFIG_DIR", "DATA_DIR", "ENV_FILE", "AGENT_FILE", "KERNEL_FILE", "VM_USER", "VM_ID_COUNT", "SYSBOX_MIN", "RUNPLANE_REPO")
		for _, c := range []struct{ name, want, file string }{
			{"CONFIG_DIR", def.ConfigDir, "internal/config/config.go"},
			{"DATA_DIR", def.DataDir, "internal/config/config.go"},
			{"ENV_FILE", def.EnvFilePath(), "internal/config/paths.go"},
			{"AGENT_FILE", def.AgentConfigPath(), "internal/config/paths.go"},
			// runplane update replaces the guest kernel at this path.
			{"KERNEL_FILE", def.KernelPath(), "internal/config/paths.go"},
			{"VM_USER", firecracker.JailerUser, "internal/sandbox/firecracker/doctor.go"},
			// The VM of TAP lease n runs with the uid and gid n of the range.
			{"VM_ID_COUNT", strconv.Itoa(netrules.TapLeases(netrules.DefaultTapSubnet)), "internal/sandbox/netrules/netrules.go"},
			{"SYSBOX_MIN", sysbox.MinVersion, "internal/sandbox/sysbox/doctor.go"},
			{"RUNPLANE_REPO", version.Repo, "internal/version/version.go"},
		} {
			agree(t, c.name+" of install.sh", vars[c.name], c.want, "install.sh", c.file)
		}
		// store_mode runs set-mode only on a primary that has a database.
		db := match(t, script, `-f "\$DATA_DIR/([^"]+)"`)
		agree(t, "the database of install.sh", filepath.Join(vars["DATA_DIR"], db), def.DBPath(), "install.sh", "internal/config/paths.go")
	})

	t.Run("runplane.env", func(t *testing.T) {
		vars := configVars(t, "-mode", "vm")
		for _, role := range roles {
			envFile := filepath.Join(t.TempDir(), "runplane.env")
			out := installSh(t, "parse_flags --dry-run "+role.flags+"\nMODE=vm\nENV_FILE="+envFile+"\nAGENT_FILE="+filepath.Join(t.TempDir(), "agent.json")+"\nwrite_config")
			env, ok := writtenFiles(out)[envFile]
			if !ok {
				t.Fatalf("install.sh of the %s writes no runplane.env:\n%s", role.name, out)
			}
			for line := range strings.Lines(env) {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				name, value, _ := strings.Cut(line, "=")
				want, ok := vars[name]
				if !ok {
					t.Errorf("install.sh of the %s writes %s to runplane.env, but runplane does not read it. Change install.sh and internal/config/config.go together.", role.name, name)
					continue
				}
				agree(t, name+" in the runplane.env of the "+role.name, value, want, "install.sh", "internal/config/config.go")
			}
		}

		listen := installSh(t, "ENV_FILE="+filepath.Join(t.TempDir(), "runplane.env")+"\nlisten_addr")
		agree(t, "the dashboard address of install.sh without runplane.env", listen, def.Listen, "install.sh", "internal/config/config.go")
		usage := match(t, installSh(t, "usage"), `--listen <addr> .*\(default: (\S+)\)`)
		agree(t, "the default --listen of install.sh --help", usage, def.Listen, "install.sh", "internal/config/config.go")

		// The script reads and changes runplane.env with sed. Its other
		// variables are its own defaults.
		own := map[string]bool{}
		for _, m := range regexp.MustCompile(`(?m)^(RUNPLANE_\w+)=\$\{`).FindAllStringSubmatch(script, -1) {
			own[m[1]] = true
		}
		names := regexp.MustCompile(`RUNPLANE_\w+`).FindAllString(script, -1)
		slices.Sort(names)
		for _, name := range slices.Compact(names) {
			if _, ok := vars[name]; !ok && !own[name] {
				t.Errorf("install.sh uses %s, which runplane does not read. Change install.sh and internal/config/config.go together.", name)
			}
		}
	})

	t.Run("systemd units", func(t *testing.T) {
		unitDir := installVars(t, "UNIT_DIR")["UNIT_DIR"]
		for _, role := range roles {
			out := installSh(t, "parse_flags --dry-run "+role.flags+"\nMODE=container\nwrite_units")
			files := writtenFiles(out)
			unit, ok := files[path.Join(unitDir, role.unit+".service")]
			if !ok {
				t.Errorf("install.sh of the %s writes no %s.service, the unit that the doctor and runplane update use. Change install.sh and cmd/runplane/doctor.go together:\n%s", role.name, role.unit, out)
				continue
			}
			command := match(t, unit, `(?m)^ExecStart=\S+ (\S+)$`)
			if cmd, ok := commands[command]; !ok || reflect.ValueOf(cmd.run).Pointer() != reflect.ValueOf(role.run).Pointer() {
				t.Errorf("%s.service of install.sh runs the command %q of runplane, which is not the one of its role. Change install.sh and cmd/runplane/main.go together.", role.unit, command)
			}
			// After an update, serve and agent exit with status 0, and only
			// systemd starts the new binary.
			if !slices.Contains(strings.Split(unit, "\n"), "Restart=always") {
				t.Errorf("%s.service of install.sh has no Restart=always, so an update stops runplane. Change install.sh, cmd/runplane/primary.go, and cmd/runplane/agent.go together.", role.unit)
			}
			if _, ok := files[path.Join(unitDir, firecracker.Slice)]; !ok {
				t.Errorf("install.sh of the %s writes no %s. Change install.sh and internal/sandbox/firecracker/cgroup.go together.", role.name, firecracker.Slice)
			}
		}
		agree(t, "the slice of sysbox", sysbox.Slice, firecracker.Slice, "internal/sandbox/sysbox/driver.go", "internal/sandbox/firecracker/cgroup.go", "install.sh")
		for _, slice := range regexp.MustCompile(`[\w-]+\.slice\b`).FindAllString(script, -1) {
			agree(t, "a slice of install.sh", slice, firecracker.Slice, "install.sh", "internal/sandbox/firecracker/cgroup.go")
		}

		units := []string{primaryUnit + ".service", agentUnit + ".service"}
		agree(t, "the units that install.sh --uninstall stops", strings.Fields(match(t, script, `for unit in (.+); do`)), units, "install.sh", "cmd/runplane/doctor.go")
		known := append(units, firecracker.Slice)
		for _, m := range regexp.MustCompile(`\$UNIT_DIR/([\w.-]+)`).FindAllStringSubmatch(script, -1) {
			if !slices.Contains(known, m[1]) {
				t.Errorf("install.sh names the unit %s, which runplane does not have. Change install.sh and cmd/runplane/doctor.go together.", m[1])
			}
		}
		for _, m := range regexp.MustCompile(`\$BIN"? ([a-z][a-z-]*)`).FindAllStringSubmatch(script, -1) {
			if _, ok := commands[m[1]]; !ok {
				t.Errorf("install.sh runs runplane %s, which is not a command. Change install.sh and cmd/runplane/main.go together.", m[1])
			}
		}
	})

	// install.sh --uninstall removes what the sandboxes leave on the host.
	t.Run("uninstall", func(t *testing.T) {
		agree(t, "the container label of install.sh --uninstall", match(t, script, `docker ps -aq --filter label=(\S+)`), sysbox.LabelSandbox, "install.sh", "internal/sandbox/sysbox/driver.go")
		agree(t, "the network label of install.sh --uninstall", match(t, script, `docker network ls -q --filter label=(\S+)`), guest.LabelManaged+"=true", "install.sh", "internal/sandbox/guest/layout.go")
		agree(t, "the TAP devices of install.sh --uninstall", match(t, script, `\\\(([^\\[]+)\[0-9\]\*\\\)`), netrules.TapPrefix, "install.sh", "internal/sandbox/netrules/netrules.go")
		tables := regexp.MustCompile(`"\w+ (\w+)"`).FindAllStringSubmatch(match(t, script, `for table in (.+); do`), -1)
		if len(tables) == 0 {
			t.Error("install.sh --uninstall removes no nftables table")
		}
		for _, m := range tables {
			agree(t, "an nftables table of install.sh --uninstall", m[1], netrules.TableName, "install.sh", "internal/sandbox/netrules/netrules.go")
		}
		agree(t, "the iptables comment of install.sh --uninstall", match(t, script, `--comment (\S+) `), netrules.RuleComment, "install.sh", "internal/sandbox/netrules/netrules.go")
		agree(t, "the iptables chains of install.sh --uninstall", strings.Fields(match(t, script, `for chain in (.+); do`)), netrules.RuleChains, "install.sh", "internal/sandbox/netrules/netrules.go")
	})

	t.Run("release files", func(t *testing.T) {
		files := []string{"install.sh", "Makefile", ".github/workflows/release.yml", "internal/update/update.go"}
		out := installSh(t, "parse_flags --dry-run --version v1.2.3\nARCH=arm64\nTMP=/tmp\nfetch_runplane")
		archive := update.ArchiveName("v1.2.3", "arm64")
		if !strings.Contains(out, "/releases/download/v1.2.3/"+archive+"\n") {
			t.Errorf("install.sh does not download %s. Change %s together:\n%s", archive, list(files), out)
		}
		makefile := repoFile(t, "Makefile")
		dist := strings.NewReplacer("$(VERSION:v%=%)", "1.2.3", "$$arch", "arm64").Replace(match(t, makefile, `-czf dist/(\S+) `))
		agree(t, "the archive of make dist", dist, archive, files...)
		release := repoFile(t, ".github/workflows/release.yml")
		checked := sh(t, `GITHUB_REF_NAME=v1.2.3; printf '%s' "`+match(t, release, `tar -xzf "dist/([^"]+)"`)+`"`)
		agree(t, "the archive that release.yml checks", checked, update.ArchiveName("v1.2.3", "amd64"), files...)

		kernels := map[string]string{"amd64": update.KernelName("amd64"), "arm64": update.KernelName("arm64")}
		installed := map[string]string{}
		for _, m := range regexp.MustCompile(`(?m)^\t(\w+)\) fc_arch=\S+ kernel_asset=(\S+) ;;$`).FindAllStringSubmatch(script, -1) {
			installed[m[1]] = m[2]
		}
		agree(t, "the guest kernels of install.sh", installed, kernels, "install.sh", "internal/update/update.go")
		var built []string
		for _, m := range regexp.MustCompile(`(?m)^\t\w+\) .*\bname=(\S+) ;;$`).FindAllStringSubmatch(repoFile(t, "kernel/build.sh"), -1) {
			built = append(built, m[1])
		}
		slices.Sort(built)
		want := slices.Sorted(maps.Values(kernels))
		agree(t, "the guest kernels of kernel/build.sh", built, want, "kernel/build.sh", "internal/update/update.go")
		var released []string
		for _, f := range strings.Fields(match(t, release, `cp ((?:kernel-out/\S+ )+)dist/`)) {
			released = append(released, path.Base(f))
		}
		slices.Sort(released)
		agree(t, "the guest kernels of release.yml", released, want, ".github/workflows/release.yml", "internal/update/update.go")
	})

	t.Run("release build", func(t *testing.T) {
		makefile := repoFile(t, "Makefile")
		pkg := match(t, makefile, `(?m)^PKG\s*:=\s*(\S+)$`)
		agree(t, "PKG of the Makefile", pkg, match(t, repoFile(t, "go.mod"), `(?m)^module (\S+)$`), "Makefile", "go.mod")
		// Go ignores -X for a variable that does not exist.
		flags := regexp.MustCompile(`-X \$\(PKG\)/(\S+)\.(\w+)=`).FindAllStringSubmatch(makefile, -1)
		if len(flags) == 0 {
			t.Error("the Makefile sets no variable with -X")
		}
		for _, m := range flags {
			if !declaresVar(t, filepath.Join("..", "..", m[1]), m[2]) {
				t.Errorf("the Makefile sets %s.%s with -X, but %s has no such variable. Change the Makefile and %s together.", m[1], m[2], m[1], m[1])
			}
		}
		agree(t, "REPO of the Makefile", match(t, makefile, `(?m)^REPO\s*\?=\s*(\S+)$`), version.Repo, "Makefile", "internal/version/version.go")
		// A binary without the embedded runplane-init starts no sandbox.
		agree(t, "the runplane-init of make init", match(t, makefile, `(?m)^\tgzip .*> (\S+)$`), path.Join("internal/initbin", initbin.File), "Makefile", "internal/initbin/initbin.go")

		// make dist puts the values of the release into these lines of
		// install.sh, and release.yml checks them.
		var set, checked []string
		for _, m := range regexp.MustCompile(`'s\|\^(RUNPLANE_\w+)=\.\*\|`).FindAllStringSubmatch(makefile, -1) {
			set = append(set, m[1])
			lines := regexp.MustCompile(`(?m)^`+m[1]+`=.*$`).FindAllString(script, -1)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], m[1]+"=${"+m[1]+":-") {
				t.Errorf("install.sh has the lines %q, want one line %s=${%s:-...}. Change install.sh and the Makefile together.", lines, m[1], m[1])
			}
		}
		for _, m := range regexp.MustCompile(`grep -qxF "(RUNPLANE_\w+)=`).FindAllStringSubmatch(repoFile(t, ".github/workflows/release.yml"), -1) {
			checked = append(checked, m[1])
		}
		agree(t, "the install.sh lines that release.yml checks", checked, set, ".github/workflows/release.yml", "Makefile")
	})

	t.Run("base image", func(t *testing.T) {
		ref := images.Ref("Acme/Fork", images.Default)
		name, tag, _ := strings.Cut(ref, ":")
		registry, repository, _ := strings.Cut(name, "/")
		out := installSh(t, `REPO=Acme/Fork
curl() {
	for a; do case $a in https://*/manifests/*) printf 'manifest %s\n' "$a" ;; esac; done
	printf '{"token":"t"}\n'
}
check_base_image`)
		agree(t, "the base image of install.sh", match(t, out, `Base image: (\S+)`), ref, "install.sh", "images/images.go")
		agree(t, "the manifest that install.sh reads", match(t, out, `manifest (\S+)`), "https://"+registry+"/v2/"+repository+"/manifests/"+tag, "install.sh", "images/images.go")

		yml := repoFile(t, ".github/workflows/images.yml")
		expand := strings.NewReplacer("${GITHUB_REPOSITORY,,}", "acme/fork", "${{ matrix.image.name }}", images.Default, "${{ matrix.image }}", images.Default)
		var pushed []string
		for line := range strings.Lines(yml) {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				pushed = append(pushed, regexp.MustCompile(`ghcr\.io/[^"]+`).FindAllString(line, -1)...)
			}
		}
		if len(pushed) == 0 {
			t.Error("images.yml pushes no image to ghcr.io")
		}
		for _, p := range pushed {
			agree(t, "an image of images.yml", expand.Replace(p), name, ".github/workflows/images.yml", "images/images.go")
		}
		if !strings.Contains(yml, `"$image:`+tag+`"`) {
			t.Errorf("images.yml does not push the tag %s. Change .github/workflows/images.yml and images/images.go together.", tag)
		}
	})

	t.Run("firecracker", func(t *testing.T) {
		installed := map[string]string{}
		for _, m := range regexp.MustCompile(`install -m 0755 "\$dir/(\w+)-\$fc_tag-\$fc_arch" (\S+)`).FindAllStringSubmatch(script, -1) {
			installed[m[1]] = m[2]
		}
		agree(t, "the programs that install.sh installs", installed, map[string]string{"firecracker": firecracker.Binary, "jailer": firecracker.JailerBinary},
			"install.sh", "internal/sandbox/firecracker/doctor.go")
		agree(t, "the firecracker whose version install.sh reads", match(t, script, `\$\((\S*firecracker) --version`), firecracker.Binary, "install.sh", "internal/sandbox/firecracker/doctor.go")
		agree(t, "the jailer that install.sh looks for", match(t, script, `\[ -x (/\S+) \]`), firecracker.JailerBinary, "install.sh", "internal/sandbox/firecracker/doctor.go")
		// The guest kernel uses the guest config of this Firecracker release.
		agree(t, "RUNPLANE_FIRECRACKER_VERSION of install.sh", installVars(t, "RUNPLANE_FIRECRACKER_VERSION")["RUNPLANE_FIRECRACKER_VERSION"],
			match(t, repoFile(t, "kernel/build.sh"), `(?m)^FIRECRACKER_REF=\$\{FIRECRACKER_REF:-(\S+)\}$`), "install.sh", "kernel/build.sh")
	})

	// An install over SSH shows the latest step of install.sh.
	t.Run("step lines", func(t *testing.T) {
		out := strings.TrimSpace(installSh(t, "step 'Write the configuration'"))
		if text, ok := strings.CutPrefix(out, fleet.StepPrefix); !ok || text != "Write the configuration" {
			t.Errorf("install.sh shows the step %q, which does not start with %q. Change install.sh and internal/fleet/install.go together.", out, fleet.StepPrefix)
		}
	})

	t.Run("dashboard status", func(t *testing.T) {
		status := func(s api.Status) string {
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		out := installSh(t, "ENV_FILE="+filepath.Join(t.TempDir(), "runplane.env")+"\ncurl() { printf '%s\\n' '"+status(api.Status{Mode: "vm"})+"'; }\nrunning_mode")
		agree(t, "the mode that install.sh reads from /api/status", strings.TrimSpace(out), "vm", "install.sh", "internal/api/setup.go")
		pattern := match(t, script, `grep -q '([^']*needs_setup[^']*)'`)
		for _, needs := range []bool{true, false} {
			out := sh(t, "if printf '%s\\n' '"+status(api.Status{NeedsSetup: needs})+"' | grep -q '"+pattern+"'; then echo true; else echo false; fi")
			agree(t, "needs_setup that install.sh reads from /api/status", strings.TrimSpace(out), strconv.FormatBool(needs), "install.sh", "internal/api/setup.go")
		}
	})
}

// agree reports a copy that differs from the value that it must have.
func agree(t *testing.T, what string, got, want any, files ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: %q, want %q. Change %s together.", what, got, want, list(files))
	}
}

// list joins file names with commas and "and".
func list(files []string) string {
	if len(files) < 3 {
		return strings.Join(files, " and ")
	}
	return strings.Join(files[:len(files)-1], ", ") + ", and " + files[len(files)-1]
}

// match returns the first group of the first match of expr in s.
func match(t *testing.T, s, expr string) string {
	t.Helper()
	m := regexp.MustCompile(expr).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match of %s", expr)
	}
	return m[1]
}

// repoFile returns a file of the repository.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// installSh runs install.sh with body in the place of its call of main, and
// returns the output.
func installSh(t *testing.T, body string) string {
	t.Helper()
	script, ok := strings.CutSuffix(strings.TrimRight(string(runplane.InstallScript), "\n"), `main "$@"`)
	if !ok {
		t.Fatal(`install.sh does not end with main "$@"`)
	}
	return sh(t, script+body+"\n")
}

// sh runs a shell script without the RUNPLANE_* variables of the
// environment, and returns its output.
func sh(t *testing.T, script string) string {
	t.Helper()
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "RUNPLANE_") })
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
	return string(out)
}

// installVars returns the values of variables of install.sh.
func installVars(t *testing.T, names ...string) map[string]string {
	t.Helper()
	body := "printf '%s\\n'"
	for _, n := range names {
		body += ` "$` + n + `"`
	}
	lines := strings.Split(strings.TrimSuffix(installSh(t, body), "\n"), "\n")
	if len(lines) != len(names) {
		t.Fatalf("values of %v: %q", names, lines)
	}
	vars := map[string]string{}
	for i, n := range names {
		vars[n] = lines[i]
	}
	return vars
}

// writtenFiles returns the files that a dry run of install.sh shows, by
// path.
func writtenFiles(out string) map[string]string {
	files := map[string]string{}
	var file string
	for line := range strings.Lines(out) {
		if p, ok := strings.CutPrefix(line, "+ write "); ok {
			file, _, _ = strings.Cut(p, " (mode ")
			files[file] = ""
		} else if l, ok := strings.CutPrefix(line, "    | "); ok && file != "" {
			files[file] += l
		} else {
			file = ""
		}
	}
	return files
}

// configVars returns the RUNPLANE_* variables of the configuration, as the
// help of each flag names them, with the value of the flag in args or the
// default.
func configVars(t *testing.T, args ...string) map[string]string {
	t.Helper()
	// Parse reads no runplane.env of this machine.
	t.Setenv("RUNPLANE_CONFIG_DIR", t.TempDir())
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if _, err := config.Parse(fs, args, config.Listen, config.AgentListen, config.ServicePort, config.DataDir, config.Mode, config.Dev, config.DevInsecure, config.SSHListen); err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`\((RUNPLANE_\w+)\)$`)
	vars := map[string]string{}
	fs.VisitAll(func(f *flag.Flag) {
		if m := name.FindStringSubmatch(f.Usage); m != nil {
			vars[m[1]] = f.Value.String()
		}
	})
	return vars
}

// declaresVar reports if the Go package in dir has the package variable
// name.
func declaresVar(t *testing.T, dir, name string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if d, ok := d.(*ast.GenDecl); ok && d.Tok == token.VAR {
				for _, s := range d.Specs {
					if slices.ContainsFunc(s.(*ast.ValueSpec).Names, func(n *ast.Ident) bool { return n.Name == name }) {
						return true
					}
				}
			}
		}
	}
	return false
}
