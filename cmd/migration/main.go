package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ritel35k/backend/internal/bootstrap"
	"github.com/ritel35k/backend/internal/platform/config"
	"github.com/ritel35k/backend/internal/platform/database"
)

const usage = `usage: migration <command>
  up                 apply all pending migrations (+ ensure audit partitions)
  down               roll back the last migration
  reset              roll back everything (DANGEROUS)
  status             show migration status
  bootstrap          create a company + owner:
                     -code ACME -name "Acme Retail" -email owner@acme.id -owner "Owner" -password '...'`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()

	switch os.Args[1] {
	case "up":
		err = database.MigrateUp(ctx, pool)
		if err == nil {
			err = database.EnsurePartitions(ctx, pool, time.Now(), 3)
		}
		if err == nil {
			err = bootstrap.SyncPermissions(ctx, pool)
		}
	case "down":
		err = database.MigrateDown(ctx, pool)
	case "reset":
		if cfg.Env == "prod" {
			err = fmt.Errorf("refusing to reset in prod")
		} else {
			err = database.MigrateReset(ctx, pool)
		}
	case "status":
		err = database.MigrateStatus(ctx, pool)
	case "bootstrap":
		fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
		code := fs.String("code", "", "company code")
		name := fs.String("name", "", "company name")
		email := fs.String("email", "", "owner email")
		owner := fs.String("owner", "Owner", "owner display name")
		pw := fs.String("password", "", "owner password (min 8)")
		_ = fs.Parse(os.Args[2:])
		if *code == "" || *name == "" || *email == "" || *pw == "" {
			fmt.Println(usage)
			os.Exit(2)
		}
		var res *bootstrap.Result
		res, err = bootstrap.CreateCompany(ctx, pool, bootstrap.Input{CompanyCode: *code, CompanyName: *name, OwnerEmail: *email, OwnerName: *owner, OwnerPassword: *pw})
		if err == nil {
			fmt.Printf("company %s created (id=%s, root_unit=%s, owner=%s)\n", *code, res.CompanyID, res.RootUnitID, res.OwnerID)
		}
	default:
		fmt.Println(usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
