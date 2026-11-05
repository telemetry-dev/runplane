package blob

import (
	"strings"
	"testing"
)

func TestValidDigest(t *testing.T) {
	hex := strings.Repeat("0123456789abcdef", 4)
	tests := []struct {
		d    string
		want bool
	}{
		{"sha256:" + hex, true},
		// The mirror names the cache files by the hex part.
		{"sha256:" + strings.ToUpper(hex), false},
		{"sha256:" + hex[:63], false},
		{"sha256:" + hex + "0", false},
		{"sha512:" + hex, false},
		{hex, false},
		{"sha256:../../etc/passwd", false},
	}
	for _, tt := range tests {
		if got := ValidDigest(tt.d); got != tt.want {
			t.Errorf("ValidDigest(%q) = %v, want %v", tt.d, got, tt.want)
		}
	}
}
