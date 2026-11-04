package update

import (
	"cmp"
	"strconv"
	"strings"
)

type semver struct {
	core [3]int
	pre  []string
}

// parse reads "v1.2.3", "1.2.3-rc.1", or "1.2.3+build".
func parse(v string) (semver, bool) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	v, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var s semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || p[0] == '+' {
			return semver{}, false
		}
		s.core[i] = n
	}
	if hasPre {
		if pre == "" {
			return semver{}, false
		}
		s.pre = strings.Split(pre, ".")
	}
	return s, true
}

// Compare compares two semantic versions and returns -1, 0, or +1.
// A version that does not parse sorts before all valid versions.
func Compare(a, b string) int {
	va, okA := parse(a)
	vb, okB := parse(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := range va.core {
		if c := cmp.Compare(va.core[i], vb.core[i]); c != 0 {
			return c
		}
	}
	// A pre-release is older than the release with the same core.
	switch {
	case len(va.pre) == 0 && len(vb.pre) == 0:
		return 0
	case len(va.pre) == 0:
		return 1
	case len(vb.pre) == 0:
		return -1
	}
	for i := 0; i < len(va.pre) && i < len(vb.pre); i++ {
		if c := comparePre(va.pre[i], vb.pre[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(va.pre), len(vb.pre))
}

// comparePre compares pre-release identifiers: numbers by value, numbers
// before words, words in ASCII order.
func comparePre(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return cmp.Compare(na, nb)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}
