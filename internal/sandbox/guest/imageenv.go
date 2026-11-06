package guest

import (
	"iter"
	"strings"
)

// ImageEnvFile holds the environment of the image config in a VM root disk,
// in the form of EncodeImageEnv. Docker gives this environment to the first
// process of a container. In a VM, runplane-init sets it from this file.
const ImageEnvFile = "/etc/runplane/environ"

// EncodeImageEnv gives the content of ImageEnvFile for the "KEY=value"
// entries of env. Each entry ends with a NUL byte.
func EncodeImageEnv(env []string) []byte {
	var b []byte
	for _, kv := range env {
		b = append(append(b, kv...), 0)
	}
	return b
}

// DecodeImageEnv gives the variables in the content of ImageEnvFile. It
// skips the entries that have no key.
func DecodeImageEnv(b []byte) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for kv := range strings.SplitSeq(string(b), "\x00") {
			if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
				if !yield(k, v) {
					return
				}
			}
		}
	}
}
