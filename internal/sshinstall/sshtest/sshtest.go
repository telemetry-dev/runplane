// Package sshtest runs an SSH server for the tests of sshinstall and its
// callers.
package sshtest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Reply gets the command of an exec request and its stdin, writes the
// output, and returns the exit status.
type Reply func(command string, stdin []byte, stdout, stderr io.Writer) uint32

// Server is an SSH server on 127.0.0.1. It accepts one client key for the
// user deploy.
type Server struct {
	Addr    string
	HostKey ssh.PublicKey
	reply   Reply

	mu       sync.Mutex
	commands []string
}

// NewServer starts a server that answers each exec request with reply. It
// stops when the test ends.
func NewServer(t testing.TB, clientKey ssh.PublicKey, reply Reply) *Server {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() == "deploy" && bytes.Equal(key.Marshal(), clientKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	config.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &Server{Addr: ln.Addr().String(), HostKey: hostKey.PublicKey(), reply: reply}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, config)
		}
	}()
	return s
}

// Commands returns the commands of the exec requests so far.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

func (s *Server) serve(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()
	_, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, reqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.exec(ch, reqs)
	}
}

func (s *Server) exec(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		ssh.Unmarshal(req.Payload, &payload)
		req.Reply(true, nil)
		s.mu.Lock()
		s.commands = append(s.commands, payload.Command)
		s.mu.Unlock()
		stdin, _ := io.ReadAll(ch)
		status := s.reply(payload.Command, stdin, ch, ch.Stderr())
		ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, status))
		return
	}
}
