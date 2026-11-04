package main

// Blank imports keep the planned dependencies in go.mod until real code uses them.
import (
	_ "github.com/Microsoft/hcsshim/ext4/tar2ext4"
	_ "github.com/actions/scaleset"
	_ "github.com/actions/scaleset/listener"
	_ "github.com/coder/websocket"
	_ "github.com/creack/pty"
	_ "github.com/danielgtaylor/huma/v2"
	_ "github.com/danielgtaylor/huma/v2/adapters/humago"
	_ "github.com/danielgtaylor/huma/v2/sse"
	_ "github.com/google/go-containerregistry/pkg/v1/remote"
	_ "github.com/hashicorp/yamux"
	_ "github.com/moby/moby/client"
	_ "github.com/oklog/ulid/v2"
	_ "golang.org/x/crypto/acme/autocert"
	_ "golang.org/x/crypto/argon2"
	_ "golang.org/x/crypto/ssh"
	_ "golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)
