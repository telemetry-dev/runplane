package host

import (
	"strings"
	"testing"
)

func TestFormatDoctor(t *testing.T) {
	var b strings.Builder
	err := FormatDoctor(&b, []Check{
		{ID: "kvm", Name: "KVM", Status: StatusOK, Detail: "/dev/kvm opens read-write", Fix: "not shown", Command: "not shown"},
		{ID: "clock", Name: "Time synchronization", Status: StatusWarn, Detail: "The kernel clock is not synchronized",
			Fix: "Turn on time sync.", Command: "sudo timedatectl set-ntp true"},
		{ID: "sysbox", Name: "Sysbox", Status: StatusFail, Detail: "Sysbox is not installed", Fix: "Install Sysbox"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `✓ KVM                   /dev/kvm opens read-write
! Time synchronization  The kernel clock is not synchronized
                        Fix: Turn on time sync.
                        Run: sudo timedatectl set-ntp true
✗ Sysbox                Sysbox is not installed
                        Fix: Install Sysbox

1 failure, 1 warning.
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestFormatDoctorAllPassed(t *testing.T) {
	var b strings.Builder
	if err := FormatDoctor(&b, []Check{{Name: "KVM", Status: StatusOK, Detail: "ok"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), "\nAll checks passed.\n") {
		t.Errorf("got %q", b.String())
	}
}

func TestWorst(t *testing.T) {
	ok, warn, fail := Check{Status: StatusOK}, Check{Status: StatusWarn}, Check{Status: StatusFail}
	tests := []struct {
		checks []Check
		want   Status
	}{
		{nil, StatusOK},
		{[]Check{ok, warn, ok}, StatusWarn},
		{[]Check{fail, warn}, StatusFail},
		{[]Check{warn, fail}, StatusFail},
	}
	for _, tt := range tests {
		if got := Worst(tt.checks); got != tt.want {
			t.Errorf("%v: got %s, want %s", tt.checks, got, tt.want)
		}
	}
}
