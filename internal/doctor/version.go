package doctor

import (
	"context"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Version is a major.minor.patch number. Missing parts are 0.
type Version [3]int

// ParseVersion reads the leading numbers of "v1.13.1", "29.5.0-rc.1", or
// "6.8.0-48-generic". It needs at least a major and a minor number.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexFunc(s, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(strings.TrimSuffix(s, "."), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Version{}, false
	}
	var v Version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v Version) Less(w Version) bool { return slices.Compare(v[:], w[:]) < 0 }

// AtLeast reports if have is a version and not older than want.
func AtLeast(have, want string) bool {
	h, ok := ParseVersion(have)
	w, _ := ParseVersion(want)
	return ok && !h.Less(w)
}

// ToolVersion runs "path --version" and returns the first version number in
// the output, or "" if the tool is missing.
func ToolVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	return firstVersion(string(out))
}

// firstVersion returns the first version number in the output of a
// "--version" flag, without a leading "v".
func firstVersion(out string) string {
	for _, word := range strings.Fields(out) {
		if _, ok := ParseVersion(word); ok {
			return strings.TrimPrefix(word, "v")
		}
	}
	return ""
}
