package host

import (
	"strconv"
	"strings"
)

// version is a major.minor.patch number. Missing parts are 0.
type version [3]int

// parseVersion reads the leading numbers of "v1.13.1", "29.5.0-rc.1", or
// "6.8.0-48-generic". It needs at least a major and a minor number.
func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexFunc(s, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(strings.TrimSuffix(s, "."), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v version) less(w version) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}
	return false
}

// atLeast reports if have is a version and not older than want.
func atLeast(have, want string) bool {
	h, ok := parseVersion(have)
	w, _ := parseVersion(want)
	return ok && !h.less(w)
}

// firstVersion returns the first version number in the output of a
// "--version" flag, without a leading "v".
func firstVersion(out string) string {
	for _, word := range strings.Fields(out) {
		if _, ok := parseVersion(word); ok {
			return strings.TrimPrefix(word, "v")
		}
	}
	return ""
}
