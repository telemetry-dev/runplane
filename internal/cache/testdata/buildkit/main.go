// Command buildkit saves and restores cache entries with the client library
// of BuildKit's type=gha cache. It reads ACTIONS_RESULTS_URL,
// ACTIONS_CACHE_SERVICE_V2, and ACTIONS_RUNTIME_TOKEN, and prints OK.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	actionscache "github.com/tonistiigi/go-actions-cache"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Println("FAIL", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func run(ctx context.Context) error {
	c, err := actionscache.TryEnv(actionscache.Opt{})
	if err != nil || c == nil {
		return fmt.Errorf("client from environment: %v", err)
	}

	small := []byte("small layer")
	big := bytes.Repeat([]byte("0123456789abcdef"), 5<<20/16) // 5 MiB, above the 1 MiB SDK block size
	for key, data := range map[string][]byte{"layer-small": small, "layer-big": big} {
		if err := c.Save(ctx, key, actionscache.NewBlob(data)); err != nil {
			return fmt.Errorf("save %s: %w", key, err)
		}
		e, err := c.Load(ctx, key)
		if err != nil || e == nil {
			return fmt.Errorf("load %s: %v %v", key, e, err)
		}
		var got bytes.Buffer
		if err := e.WriteTo(ctx, &got); err != nil {
			return fmt.Errorf("download %s: %w", key, err)
		}
		if !bytes.Equal(got.Bytes(), data) {
			return fmt.Errorf("download %s: %d bytes differ", key, got.Len())
		}
		// BuildKit reads layers at offsets.
		part := make([]byte, 4)
		rac := e.Download(ctx)
		if _, err := rac.ReadAt(part, int64(len(data)-4)); err != nil && err != io.EOF {
			return fmt.Errorf("read %s at offset: %w", key, err)
		}
		rac.Close()
		if !bytes.Equal(part, data[len(data)-4:]) {
			return fmt.Errorf("read %s at offset: got %q", key, part)
		}
	}

	// BuildKit skips a layer when the save of an existing key fails with os.ErrExist.
	if err := c.Save(ctx, "layer-small", actionscache.NewBlob(small)); !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("save existing key: %v, want os.ErrExist", err)
	}

	// The cache index uses numbered keys and a prefix lookup.
	for i := range 2 {
		err := c.SaveMutable(ctx, "index", 10*time.Second, func(*actionscache.Entry) (actionscache.Blob, error) {
			return actionscache.NewBlob([]byte(fmt.Sprint("index ", i))), nil
		})
		if err != nil {
			return fmt.Errorf("save index %d: %w", i, err)
		}
	}
	e, err := c.Load(ctx, "index#")
	if err != nil || e == nil || e.Key != "index#2" {
		return fmt.Errorf("load newest index: %+v %v", e, err)
	}
	return nil
}
