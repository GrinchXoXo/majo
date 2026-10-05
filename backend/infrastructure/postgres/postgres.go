// Package postgres wraps database connectivity and migration execution.
// PostgreSQL is the system of record for Middleman (per PRD 0.2 §8-10):
// if losing another store would permanently lose business data, that data
// belongs here instead.
//
// Uses database/sql with lib/pq, a dependency-free driver, to keep the
// module graph simple during Phase 0.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// Connect establishes a connection pool to PostgreSQL and verifies
// connectivity with a ping before returning. Failing fast here means
// the application never starts in a half-initialized state.
func Connect(ctx context.Context, databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	return db, nil
}
