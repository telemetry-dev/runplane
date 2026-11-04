package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/runplane/runplane/internal/sandbox"
)

type nopHandler struct{}

func (nopHandler) OnEvent(sandbox.RunnerEvent) {}
func (nopHandler) OnLog(LogLine)               {}
func (nopHandler) OnLostConfig()               {}

func TestTCPListenerChecksToken(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	tl, err := ListenTCP("127.0.0.1:0", "127.0.0.1", log)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	dir := t.TempDir()
	cfg := GuestConfig{SandboxID: "01jbz3x0000000000000000001", JITConfig: "jit"}
	srv, err := tl.Listen(dir, cfg, nopHandler{}, log)
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(filepath.Join(dir, AddrFile))
	if err != nil {
		t.Fatal(err)
	}
	addr, hello, err := ParseAddr(content)
	if err != nil {
		t.Fatal(err)
	}
	if want := tl.Addr().String(); addr != want {
		t.Fatalf("control.addr has %s, the listener is at %s", addr, want)
	}

	for _, bad := range []string{
		"RUNPLANE " + cfg.SandboxID + " " + strings.Repeat("0", 64) + "\n",
		"RUNPLANE 01jbz3x0000000000000000002 " + strings.Fields(hello)[2] + "\n",
		strings.Replace(hello, "RUNPLANE", "HELLO", 1),
	} {
		conn := dialTCP(t, addr, bad)
		if !closedByPeer(conn) {
			t.Errorf("the host kept a connection with the hello line %q", bad)
		}
		conn.Close()
	}
	if srv.Connected() {
		t.Fatal("a guest with a wrong hello line got connected")
	}

	conn := dialTCP(t, addr, hello)
	got := getConfig(t, conn)
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("config = %+v, want %+v", got, cfg)
	}
	if !srv.Connected() {
		t.Error("Connected() = false after a good hello line")
	}

	// A closed sandbox refuses its old token.
	srv.Close()
	conn = dialTCP(t, addr, hello)
	if !closedByPeer(conn) {
		t.Error("the host kept a connection for a closed sandbox")
	}
}

func dialTCP(t *testing.T, addr, hello string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, hello); err != nil {
		t.Fatal(err)
	}
	return conn
}

func closedByPeer(conn net.Conn) bool {
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	return err == io.EOF
}

func getConfig(t *testing.T, conn net.Conn) GuestConfig {
	t.Helper()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
	}}
	resp, err := client.Get("http://host/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got GuestConfig
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}
