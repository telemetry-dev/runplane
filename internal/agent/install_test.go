package agent

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runplane/runplane/internal/installtest"
)

// install.sh writes agent.json for a new agent, and on a later run it reads
// the file that the agent wrote after its registration. Change install.sh
// and agentFile together.
func TestInstallScriptAgentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	fp := "sha256:" + strings.Repeat("0f", 32)
	flags := "--token rp_srv_TEST --primary 203.0.113.7:8485 --fingerprint " + fp
	install := func(flags string) string {
		t.Helper()
		return installtest.Run(t, fmt.Sprintf(`CONFIG_DIR=%[1]s DATA_DIR=%[1]s/data UNIT_DIR=%[1]s AGENT_FILE=%[2]s ENV_FILE=%[3]s
parse_flags %[4]s
check_role
MODE=container
write_config
printf 'primary=%%s\n' "$PRIMARY"`, dir, path, filepath.Join(t.TempDir(), "runplane.env"), flags))
	}

	install(flags)
	f, err := readAgentFile(path)
	if err != nil {
		t.Fatalf("the agent cannot read the agent.json of install.sh: %v", err)
	}
	if want := (agentFile{Primary: "203.0.113.7:8485", Fingerprint: fp, Token: "rp_srv_TEST"}); f != want {
		t.Errorf("the agent.json of install.sh gives %+v, want %+v", f, want)
	}

	f.Token, f.ServerID, f.Secret = "", "01jbd5x7k2m4n6p8q9r0s1t2v3", "s3cret"
	if err := f.write(path); err != nil {
		t.Fatal(err)
	}
	registered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The same install command again, and an update without a token.
	for _, flags := range []string{flags, ""} {
		out := install(flags)
		if !strings.Contains(out, "primary="+f.Primary+"\n") {
			t.Errorf("install.sh %s does not find the primary in the agent.json of the agent:\n%s", flags, out)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, registered) {
			t.Errorf("install.sh %s changed the agent.json of a registered agent:\n%s\nwant\n%s", flags, got, registered)
		}
	}
}
