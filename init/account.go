package main

import (
	"fmt"
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

func (a account) env(extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "TERM=") })
	if os.Getenv("LANG") == "" {
		env = append(env, "LANG=C.UTF-8")
	}
	env = append(env, "HOME="+a.home, "USER="+a.name, "LOGNAME="+a.name, "SHELL="+a.shell)
	return append(env, extra...)
}
