package guest

import (
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/runplane/runplane/images"
)

// The builtin images make the runner user and the folders that runplane-init
// uses, and have the label that finds them on a server. A change to one of
// these constants must change images/*/Dockerfile too, and the other way
// around.
func TestBuiltinImagesHaveTheGuestLayout(t *testing.T) {
	files := map[string]string{}
	err := fs.WalkDir(images.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && path.Base(p) == "Dockerfile" {
			b, err := fs.ReadFile(images.FS, p)
			files[p] = string(b)
			return err
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("images.FS has no Dockerfile")
	}
	for name, text := range files {
		t.Run(name, func(t *testing.T) {
			// Join the continued lines, and split the RUN steps into commands.
			text = strings.ReplaceAll(text, "\\\n", " ")
			var cmds [][]string
			for _, c := range regexp.MustCompile(`[;\n]|&&`).Split(text, -1) {
				if f := strings.Fields(c); len(f) > 0 {
					cmds = append(cmds, f)
				}
			}
			find := func(name string) [][]string {
				var found [][]string
				for _, c := range cmds {
					if i := slices.Index(c, name); i >= 0 {
						found = append(found, c[i:])
					}
				}
				return found
			}

			if !slices.ContainsFunc(find("LABEL"), func(c []string) bool { return slices.Contains(c, LabelManaged+"=true") }) {
				t.Errorf("no LABEL %s=true", LabelManaged)
			}
			users := find("useradd")
			if len(users) != 1 {
				t.Fatalf("%d useradd commands, want 1", len(users))
			}
			user := users[0]
			if name := user[len(user)-1]; name != RunnerUser {
				t.Errorf("useradd makes the user %q, want %q", name, RunnerUser)
			}
			if i := slices.Index(user, "--uid"); i < 0 || i+1 >= len(user) || user[i+1] != strconv.Itoa(RunnerUID) {
				t.Errorf("useradd %v, want --uid %d", user[1:], RunnerUID)
			}
			for _, dir := range []string{CtlDir, ScratchDir, RunnerHome} {
				if !slices.ContainsFunc(find("mkdir"), func(c []string) bool { return slices.Contains(c, dir) }) {
					t.Errorf("no mkdir of %s", dir)
				}
			}
			owner := RunnerUser + ":" + RunnerUser
			if !slices.ContainsFunc(find("chown"), func(c []string) bool { return slices.Contains(c, owner) && slices.Contains(c, RunnerHome) }) {
				t.Errorf("no chown %s %s", owner, RunnerHome)
			}
		})
	}
}
