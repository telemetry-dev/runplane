//go:build linux

package main

import (
	_ "github.com/google/nftables"
	_ "github.com/mdlayher/vsock"
	_ "github.com/vishvananda/netlink"
)
