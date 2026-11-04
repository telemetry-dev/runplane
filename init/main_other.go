//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "runplane-init runs only on Linux")
	os.Exit(1)
}
