// Package installtest runs install.sh, or functions of it, for the tests of
// the Go code that install.sh must agree with.
package installtest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/runplane/runplane"
)

// Run runs install.sh with body in the place of its call of main, and
// returns the output. A failure of the script ends the test.
func Run(t testing.TB, body string) string {
	t.Helper()
	script, err := ReplaceMain(runplane.InstallScript, body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Sh(script, nil, "-s")
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	return out
}

// ReplaceMain returns script, a copy of install.sh, with body in the place
// of its call of main. The body can call the functions of the script.
func ReplaceMain(script []byte, body string) ([]byte, error) {
	src, ok := strings.CutSuffix(strings.TrimRight(string(script), "\n"), `main "$@"`)
	if !ok {
		return nil, errors.New(`install.sh does not end with main "$@"`)
	}
	return []byte(src + body + "\n"), nil
}

// Sh runs sh with args and stdin, and returns the output. The environment
// has env, but not the RUNPLANE_* variables of the test process: install.sh
// reads them, and the developer can have them in the shell.
func Sh(stdin []byte, env []string, args ...string) (string, error) {
	cmd := exec.Command("sh", args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "RUNPLANE_") }), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
