package main

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// account is a user from /etc/passwd with its groups from /etc/group.
type account struct {
	name     string
	uid, gid uint32
	groups   []uint32 // more groups, for example docker and sudo
	home     string
	shell    string
}

func lookupAccount(name, passwdPath, groupPath string) (account, error) {
	passwd, err := os.ReadFile(passwdPath)
	if err != nil {
		return account{}, fmt.Errorf("read passwd: %w", err)
	}
	a := account{name: name}
	found := false
	for line := range strings.Lines(string(passwd)) {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 7 || f[0] != name {
			continue
		}
		uid, err1 := strconv.ParseUint(f[2], 10, 32)
		gid, err2 := strconv.ParseUint(f[3], 10, 32)
		if err1 != nil || err2 != nil {
			return account{}, fmt.Errorf("bad passwd entry for %s", name)
		}
		a.uid, a.gid, a.home, a.shell = uint32(uid), uint32(gid), f[5], f[6]
		found = true
		break
	}
	if !found {
		return account{}, fmt.Errorf("user %s is not in %s", name, passwdPath)
	}
	if a.shell == "" {
		a.shell = "/bin/sh"
	}

	group, err := os.ReadFile(groupPath)
	if err != nil {
		return account{}, fmt.Errorf("read group: %w", err)
	}
	for line := range strings.Lines(string(group)) {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 4 || !slices.Contains(strings.Split(f[3], ","), name) {
			continue
		}
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err == nil && uint32(gid) != a.gid {
			a.groups = append(a.groups, uint32(gid))
		}
	}
	return a, nil
}

// env returns the environment for a process of the account: the init
// environment without TERM, with the account's HOME, USER, LOGNAME, and SHELL.
func (a account) env(extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "TERM=") })
	if os.Getenv("LANG") == "" {
		env = append(env, "LANG=C.UTF-8")
	}
	env = append(env, "HOME="+a.home, "USER="+a.name, "LOGNAME="+a.name, "SHELL="+a.shell)
	return append(env, extra...)
}

// writeEnvFile writes the runner .env file. The runner sets each
// "key=value" line as an environment variable, so it skips an entry that
// cannot fit on one line.
func writeEnvFile(path string, env map[string]string, log *slog.Logger) error {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(env)) {
		v := env[k]
		if k == "" || strings.ContainsAny(k, "=\r\n") || strings.ContainsAny(v, "\r\n") {
			log.Warn("skipping a runner environment variable that does not fit in .env", "key", k)
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
