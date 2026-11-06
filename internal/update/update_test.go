package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// testdata holds a release made the same way as the release workflow:
// SHA256SUMS.sig is "openssl pkeyutl -sign -rawin" with an Ed25519 key, and
// releaseKey is that key's public half.
const releaseKey = "UA5XrDu50Jft+evOhIqoRivYToV1G6Y9eDNe62YzsQg="

const otherKey = "Zm9yLXRlc3RzLW9ubHktbm90LWEtcmVsZWFzZS1rZXk="

type release struct {
	sums, sig, archive []byte
	kernels            map[string][]byte // by asset name, for example "vmlinux-arm64"
}

func loadRelease(t *testing.T) release {
	t.Helper()
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	return release{sums: read("SHA256SUMS"), sig: read("SHA256SUMS.sig"), archive: read("runplane_0.5.0_linux.tar.gz")}
}

// serve answers like github.com release downloads for acme/runplane v0.5.0.
func serve(t *testing.T, rel release) *httptest.Server {
	t.Helper()
	files := map[string][]byte{
		"/acme/runplane/releases/download/v0.5.0/SHA256SUMS":                        rel.sums,
		"/acme/runplane/releases/download/v0.5.0/SHA256SUMS.sig":                    rel.sig,
		"/acme/runplane/releases/download/v0.5.0/runplane_0.5.0_linux_amd64.tar.gz": rel.archive,
		"/acme/runplane/releases/download/v0.5.0/runplane_0.5.0_linux_arm64.tar.gz": rel.archive,
	}
	for name, data := range rel.kernels {
		files["/acme/runplane/releases/download/v0.5.0/"+name] = data
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestApply(t *testing.T) {
	good := loadRelease(t)

	otherArchive := good
	otherArchive.archive = append(bytes.Clone(good.archive), 0)

	// An attacker swaps the archive and puts its hash in SHA256SUMS, but
	// cannot sign the new SHA256SUMS.
	tampered := otherArchive
	sum := sha256.Sum256(otherArchive.archive)
	tampered.sums = bytes.ReplaceAll(good.sums, []byte("c2b1efeab0decc99293a3167957934880092c4190d9c27df797ea7d9941ec1d6"), []byte(hex.EncodeToString(sum[:])))

	tests := []struct {
		name    string
		rel     release
		key     string
		wantErr bool
	}{
		{"signed release", good, releaseKey, false},
		{"tampered SHA256SUMS", tampered, releaseKey, true},
		{"wrong key", good, otherKey, true},
		{"archive does not match SHA256SUMS", otherArchive, releaseKey, true},
		{"build without a key", good, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serve(t, tt.rel)
			exe := filepath.Join(t.TempDir(), "runplane")
			os.WriteFile(exe, []byte("old binary"), 0o755)

			u := &Updater{Repo: "acme/runplane", Current: "v0.4.2", PublicKey: tt.key, DownloadURL: srv.URL, Exe: exe}
			err := u.Apply(context.Background(), "v0.5.0")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}

			got, _ := os.ReadFile(exe)
			want := "old binary"
			if !tt.wantErr {
				want = "#!/bin/sh\necho runplane v0.5.0\n"
			}
			if string(got) != want {
				t.Fatalf("executable has %q, want %q", got, want)
			}
			info, _ := os.Stat(exe)
			if info.Mode().Perm() != 0o755 {
				t.Fatalf("executable mode %o, want 755", info.Mode().Perm())
			}
			if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
				t.Fatalf("left %d files next to the executable", len(entries))
			}
		})
	}
}

func TestApplyFollowsSymlink(t *testing.T) {
	srv := serve(t, loadRelease(t))
	dir := t.TempDir()
	real := filepath.Join(dir, "runplane-0.4.2")
	os.WriteFile(real, []byte("old binary"), 0o755)
	link := filepath.Join(dir, "runplane")
	os.Symlink(real, link)

	u := &Updater{Repo: "acme/runplane", PublicKey: releaseKey, DownloadURL: srv.URL, Exe: link}
	if err := u.Apply(context.Background(), "v0.5.0"); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(link); err != nil || target != real {
		t.Fatalf("symlink replaced: target %q, err %v", target, err)
	}
	if got, _ := os.ReadFile(real); !bytes.Contains(got, []byte("v0.5.0")) {
		t.Fatalf("symlink target not updated: %q", got)
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/runplane/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name":"v0.10.0","name":"v0.10.0","html_url":"https://github.com/acme/runplane/releases/tag/v0.10.0","body":"Fixes","draft":false,"prerelease":false,"published_at":"2026-11-01T12:00:00Z","assets":[]}`))
	}))
	defer srv.Close()

	tests := []struct {
		current string
		want    bool
	}{
		{"v0.4.2", true}, // a string comparison would say "v0.4.2" > "v0.10.0"
		{"v0.10.0", false},
		{"v0.11.0-rc.1", false},
		{"v0.10.0-rc.2", true},
		{"dev", false},
	}
	for _, tt := range tests {
		u := &Updater{Repo: "acme/runplane", Current: tt.current, APIURL: srv.URL}
		st, err := u.Check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Available != tt.want || st.Latest != "v0.10.0" || st.URL != "https://github.com/acme/runplane/releases/tag/v0.10.0" {
			t.Errorf("current %s: %+v, want available %v", tt.current, st, tt.want)
		}
	}
}

func TestApplyReplacesTheGuestKernel(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	archive := loadRelease(t).archive
	// cmd/runplane checks KernelName against the files of kernel/build.sh.
	kernels := map[string][]byte{"vmlinux-x86_64": []byte("kernel x86_64 6.1.200"), "vmlinux-arm64": []byte("kernel arm64 6.1.200")}
	want := kernels[KernelName(runtime.GOARCH)]
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	sums := func(withKernels bool, kernelData []byte) string {
		lines := sum(archive) + "  runplane_0.5.0_linux_amd64.tar.gz\n" + sum(archive) + "  runplane_0.5.0_linux_arm64.tar.gz\n"
		if withKernels {
			lines += sum(kernelData) + "  vmlinux-arm64\n" + sum(kernelData) + "  vmlinux-x86_64\n"
		}
		return lines
	}

	tests := []struct {
		name       string
		sums       string
		hasKernel  bool // the server has a guest kernel (VM mode)
		wantErr    bool
		wantKernel string // "" means no kernel file
		wantExe    string
	}{
		{"VM mode", sums(true, want), true, false, string(want), "#!/bin/sh\necho runplane v0.5.0\n"},
		{"container mode", sums(true, want), false, false, "", "#!/bin/sh\necho runplane v0.5.0\n"},
		{"release without a kernel", sums(false, nil), true, true, "old kernel", "old binary"},
		{"kernel does not match SHA256SUMS", sums(true, []byte("other kernel")), true, true, "old kernel", "old binary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serve(t, release{sums: []byte(tt.sums), sig: ed25519.Sign(priv, []byte(tt.sums)), archive: archive, kernels: kernels})

			exe := filepath.Join(t.TempDir(), "runplane")
			os.WriteFile(exe, []byte("old binary"), 0o755)
			kernelDir := t.TempDir()
			kernel := filepath.Join(kernelDir, "vmlinux")
			if tt.hasKernel {
				os.WriteFile(kernel, []byte("old kernel"), 0o644)
			}

			u := &Updater{Repo: "acme/runplane", PublicKey: base64.StdEncoding.EncodeToString(pub), DownloadURL: srv.URL, Exe: exe, Kernel: kernel}
			err := u.Apply(context.Background(), "v0.5.0")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if got, _ := os.ReadFile(exe); string(got) != tt.wantExe {
				t.Errorf("executable has %q, want %q", got, tt.wantExe)
			}
			got, err := os.ReadFile(kernel)
			switch {
			case tt.wantKernel == "" && err == nil:
				t.Errorf("Apply made a guest kernel %q on a server without one", got)
			case tt.wantKernel != "" && string(got) != tt.wantKernel:
				t.Errorf("guest kernel has %q, want %q", got, tt.wantKernel)
			}
			if info, err := os.Stat(kernel); err == nil && info.Mode().Perm() != 0o644 {
				t.Errorf("guest kernel mode %o, want 644", info.Mode().Perm())
			}
			if entries, _ := os.ReadDir(kernelDir); len(entries) > 1 {
				t.Errorf("left %d files next to the guest kernel", len(entries))
			}
		})
	}
}
