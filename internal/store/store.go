package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed queries.sql import.sql schema.sql migrations/*.sql
var files embed.FS
var queries = loadQueries()

// Migration versions are consecutive starting at 1, matching schema_migrations.
// Discover embedded files rather than maintaining a second list in Go code.
func migrationPaths(source fs.FS) ([]string, error) {
	paths, err := fs.Glob(source, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("no SQL migrations found")
	}
	versions := make(map[string]int, len(paths))
	for _, file := range paths {
		number, description, ok := strings.Cut(strings.TrimSuffix(path.Base(file), ".sql"), "_")
		if !ok || description == "" || number == "" || strings.Trim(number, "0123456789") != "" {
			return nil, fmt.Errorf("invalid migration filename: %s", file)
		}
		version, err := strconv.ParseInt(number, 10, 32)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version: %s", file)
		}
		versions[file] = int(version)
	}
	slices.SortFunc(paths, func(a, b string) int { return versions[a] - versions[b] })
	for i, file := range paths {
		if versions[file] != i+1 {
			return nil, fmt.Errorf("migration versions must be unique and consecutive from 1: %s", file)
		}
	}
	return paths, nil
}

func loadQueries() map[string]string {
	result := map[string]string{}
	for _, file := range []string{"queries.sql", "import.sql", "schema.sql"} {
		data, err := files.ReadFile(file)
		if err != nil {
			panic(err)
		}
		for _, part := range strings.Split(string(data), "-- name: ")[1:] {
			name, sql, _ := strings.Cut(part, "\n")
			result[strings.TrimSpace(name)] = strings.TrimSpace(sql)
		}
	}
	return result
}
func Q(name string) string {
	q, ok := queries[name]
	if !ok {
		panic("unknown query: " + name)
	}
	return q
}

func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("database initialization failed")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("database unavailable")
	}
	return pool, nil
}
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := migrationPaths(files)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, Q("migration_lock")); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, Q("migration_bootstrap")); err != nil {
		return err
	}
	var latest int
	if err = tx.QueryRow(ctx, Q("migration_latest")).Scan(&latest); err != nil {
		return err
	}
	if latest > len(migrations) {
		return errors.New("incompatible schema")
	}
	for i, path := range migrations {
		sql, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(sql)
		checksum := hex.EncodeToString(sum[:])
		var existing string
		err = tx.QueryRow(ctx, Q("migration_checksum"), i+1).Scan(&existing)
		if err == nil {
			if existing != checksum {
				return errors.New("migration checksum mismatch")
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, Q("migration_record"), i+1, checksum); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := migrationPaths(files)
	if err != nil {
		return err
	}
	var n int
	if err := pool.QueryRow(ctx, Q("migration_check")).Scan(&n); err != nil {
		return errors.New("run migrate before startup")
	}
	if n != len(migrations) {
		return errors.New("incompatible schema")
	}
	for i, path := range migrations {
		sql, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(sql)
		var checksum string
		if err = pool.QueryRow(ctx, Q("migration_checksum"), i+1).Scan(&checksum); err != nil || checksum != hex.EncodeToString(sum[:]) {
			return errors.New("incompatible schema")
		}
	}
	return nil
}
