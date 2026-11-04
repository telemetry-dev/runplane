package main

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

// etcHosts returns the /etc/hosts of a sandbox from the file of the image.
// An exported image has an empty file, and the DNS servers do not know
// localhost. Thus etcHosts adds the localhost lines that Docker writes when
// no line names localhost. It also adds the host name, which sudo looks up.
func etcHosts(image []byte, hostname string) []byte {
	var b bytes.Buffer
	if !namesLocalhost(image) {
		b.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	}
	b.Write(image)
	if len(image) > 0 && image[len(image)-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "127.0.1.1\t%s\n", hostname)
	return b.Bytes()
}

func namesLocalhost(hosts []byte) bool {
	for line := range strings.Lines(string(hosts)) {
		line, _, _ = strings.Cut(line, "#")
		if f := strings.Fields(line); len(f) > 1 && slices.Contains(f[1:], "localhost") {
			return true
		}
	}
	return false
}
