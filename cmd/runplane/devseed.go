//go:build devseed

package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/devseed"
	"github.com/runplane/runplane/internal/secret"
	"github.com/runplane/runplane/internal/store"
)

func init() {
	commands["dev-seed"] = command{"fill an empty database with demo data (development only)", devSeed}
}

func devSeed(ctx context.Context, args []string) error {
	cfg, err := config.Parse(flag.NewFlagSet("dev-seed", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	key, err := secret.LoadOrCreate(cfg.SecretKeyPath())
	if err != nil {
		return err
	}
	box, err := secret.NewBox(key)
	if err != nil {
		return err
	}
	if err := devseed.Seed(ctx, st, box, cfg.LogsDir(), time.Now()); err != nil {
		return err
	}
	fmt.Println("Seeded", cfg.DBPath(), "- log in as ada@kestrel.dev with password runplane-dev-password")
	return nil
}
