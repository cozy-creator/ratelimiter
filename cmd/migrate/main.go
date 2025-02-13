package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/uptrace/bun/driver/pgdriver"
)

func main() {
	// Connect to database
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/ratelimiter?sslmode=disable"
	}

	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	defer db.Close()

	// Create migrations table if it doesn't exist
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		log.Fatalf("creating migrations table: %v", err)
	}

	// Get applied migrations
	rows, err := db.Query("SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		log.Fatalf("getting applied migrations: %v", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			log.Fatalf("scanning migration version: %v", err)
		}
		applied[version] = true
	}

	// Read migration files
	migrationFiles, err := filepath.Glob("migrations/sql/*.up.sql")
	if err != nil {
		log.Fatalf("finding migration files: %v", err)
	}

	// Apply migrations in order
	for _, file := range migrationFiles {
		version := strings.TrimSuffix(filepath.Base(file), ".up.sql")
		if applied[version] {
			continue
		}

		// Read migration file
		content, err := os.ReadFile(file)
		if err != nil {
			log.Fatalf("reading migration file %s: %v", file, err)
		}

		// Begin transaction
		tx, err := db.Begin()
		if err != nil {
			log.Fatalf("beginning transaction: %v", err)
		}

		// Apply migration
		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			log.Fatalf("applying migration %s: %v", file, err)
		}

		// Record migration
		if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			tx.Rollback()
			log.Fatalf("recording migration %s: %v", file, err)
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Fatalf("committing transaction: %v", err)
		}

		fmt.Printf("Applied migration: %s\n", version)
	}
} 