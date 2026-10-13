// Command operatorctl manages named operator credentials (migration 0016).
//
//	DATABASE_URL=... go run ./cmd/operatorctl create -name "Ada" -role OPERATOR
//	DATABASE_URL=... go run ./cmd/operatorctl list
//	DATABASE_URL=... go run ./cmd/operatorctl revoke -name "Ada"
//
// A new token is printed once and never stored: only its SHA-256 hash is kept.
// Run the server once first so migrations are applied.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/middleman/backend/infrastructure/postgres"
	"github.com/middleman/backend/internal/access"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fail("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := postgres.Connect(ctx, dsn)
	if err != nil {
		fail("connect: %v", err)
	}
	defer db.Close()
	repo := postgres.NewOperatorRepository(db)

	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	name := fs.String("name", "", "operator name (unique)")
	role := fs.String("role", "OPERATOR", "VIEWER or OPERATOR")
	_ = fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "create":
		o, token, err := repo.Create(ctx, *name, access.Role(strings.ToUpper(*role)))
		if err != nil {
			fail("create: %v", err)
		}
		fmt.Printf("created operator %q (%s), id %s\n", o.Name, o.Role, o.ID)
		fmt.Printf("token (shown once; give it to the operator, then discard): %s\n", token)
	case "revoke":
		if err := repo.Revoke(ctx, *name); err != nil {
			fail("revoke: %v", err)
		}
		fmt.Printf("revoked operator %q\n", *name)
	case "list":
		ops, err := repo.List(ctx)
		if err != nil {
			fail("list: %v", err)
		}
		for _, o := range ops {
			state := "active"
			if !o.Active {
				state = "revoked"
			}
			last := "never"
			if o.LastUsedAt != nil {
				last = o.LastUsedAt.UTC().Format(time.RFC3339)
			}
			fmt.Printf("%-24s %-9s %-8s last used %s\n", o.Name, o.Role, state, last)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: operatorctl create -name NAME [-role VIEWER|OPERATOR] | list | revoke -name NAME")
	os.Exit(2)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "operatorctl: "+format+"\n", a...)
	os.Exit(1)
}
