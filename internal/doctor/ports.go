package doctor

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type portScan struct {
	addr string
	err  error // from listening on addr; nil when the port is free
	pid  int   // the process that listens, when known
	name string
	self bool // this process listens
}

func portChecks(ports []portScan, root bool, envFile string) []Check {
	var cs []Check
	for _, p := range ports {
		_, port, _ := net.SplitHostPort(p.addr)
		port = cmp.Or(port, p.addr)
		c := Check{ID: "port_" + port, Name: "Port " + port}
		inUse := errors.Is(p.err, syscall.EADDRINUSE)
		switch {
		case p.err == nil:
			c = c.OK(p.addr + " is free")
		case inUse && (p.self || p.name == "runplane"):
			c = c.OK("runplane listens on " + p.addr)
		case inUse && p.name != "":
			c = c.Fail(fmt.Sprintf("%s (pid %d) listens on %s", p.name, p.pid, p.addr), "Stop "+p.name+", or give runplane a different port.")
		case inUse && !root:
			c = c.AsRoot("Another process listens on " + p.addr)
		case inUse:
			c = c.Fail("Another process listens on "+p.addr, "Stop that process, or give runplane a different port.")
		case errors.Is(p.err, syscall.EACCES):
			c = c.Warn("Ports below 1024 need root", "Run runplane as root.")
		default:
			c = c.Warn(fmt.Sprintf("Cannot listen on %s: %v", p.addr, p.err), "Check the listen address in "+envFile+".")
		}
		cs = append(cs, c)
	}
	return cs
}

func scanPorts(addrs []string, own bool) []portScan {
	var ports []portScan
	for _, addr := range addrs {
		ports = append(ports, scanPort(addr, own))
	}
	return ports
}

// scanPort tries to listen on addr. own tells that this process listens on
// it already.
func scanPort(addr string, own bool) portScan {
	p := portScan{addr: addr}
	l, err := net.Listen("tcp", addr)
	if err == nil {
		l.Close()
		return p
	}
	p.err = err
	if !errors.Is(err, syscall.EADDRINUSE) {
		return p
	}
	if own {
		p.self = true
		return p
	}
	_, port, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(port)
	p.pid, p.name = portOwner(n)
	p.self = p.pid == os.Getpid()
	return p
}

// listenInodes returns the inodes of the sockets that listen on port in a
// /proc/net/tcp or /proc/net/tcp6 table.
func listenInodes(r io.Reader, port int) []string {
	const tcpListen = "0A"
	var inodes []string
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 10 || f[3] != tcpListen {
			continue
		}
		_, hexPort, _ := strings.Cut(f[1], ":")
		if p, err := strconv.ParseUint(hexPort, 16, 16); err == nil && int(p) == port {
			inodes = append(inodes, f[9])
		}
	}
	return inodes
}
