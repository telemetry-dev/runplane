package scheduler

import "testing"

// The github package finds the runners of a scale set by IsRunnerOf. A
// name that does not match its label makes the cleanup miss stray runners
// with no error.
func TestRunnerNameIsRunnerOfItsLabel(t *testing.T) {
	name := RunnerName("runplane-4cpu", "01jbz4v5k2n8q3r6t9w0x1y2z3")
	for _, tt := range []struct {
		label string
		want  bool
	}{
		{"runplane-4cpu", true},
		{"RunPlane-4CPU", true}, // GitHub compares without case
		{"runplane-8cpu", false},
		{"runplane-4c", false},
	} {
		if got := IsRunnerOf(name, tt.label); got != tt.want {
			t.Errorf("IsRunnerOf(%q, %q) = %v, want %v", name, tt.label, got, tt.want)
		}
	}
}
