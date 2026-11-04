package main

import "testing"

func TestEtcHosts(t *testing.T) {
	tests := []struct {
		name, image, want string
	}{
		{
			name:  "empty file of an exported image",
			image: "",
			want:  "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\trunplane-0c5ta\n",
		},
		{
			name:  "image with localhost keeps its lines",
			image: "127.0.0.1 localhost\n::1 ip6-localhost ip6-loopback\n",
			want:  "127.0.0.1 localhost\n::1 ip6-localhost ip6-loopback\n127.0.1.1\trunplane-0c5ta\n",
		},
		{
			name:  "localhost as an alias, no newline at the end",
			image: "127.0.0.1\tlocalhost.localdomain localhost",
			want:  "127.0.0.1\tlocalhost.localdomain localhost\n127.0.1.1\trunplane-0c5ta\n",
		},
		{
			name:  "localhost only in a comment or as an address",
			image: "# 127.0.0.1 localhost\nlocalhost\n10.0.0.1 db\n",
			want:  "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n# 127.0.0.1 localhost\nlocalhost\n10.0.0.1 db\n127.0.1.1\trunplane-0c5ta\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(etcHosts([]byte(tt.image), "runplane-0c5ta")); got != tt.want {
				t.Errorf("etcHosts(%q) =\n%q\nwant\n%q", tt.image, got, tt.want)
			}
		})
	}
}
