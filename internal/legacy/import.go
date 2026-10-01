// Package legacy reads an offline SQLite snapshot; it never imports application code or writes SQLite.
package legacy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite"
	"registration.local/backend/internal/domain"
	"registration.local/backend/internal/store"
)

//go:embed queries.sql
var sqlText string

func query(name string) string {
	for _, p := range strings.Split(sqlText, "-- name: ")[1:] {
		n, q, _ := strings.Cut(p, "\n")
		if n == name {
			return q
		}
	}
	panic("unknown legacy query")
}

type Report struct {
	Users, Permissions, Chats, ArchivedMessages int
	InvalidFields                               map[string]int
	MissingFields                               map[string]int
	Hash                                        string
	AlreadyImported                             bool
}
type Snapshot struct {
	Report                    Report
	Users, Permissions, Chats [][]any
}

func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open snapshot")
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", errors.New("cannot hash snapshot")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func date(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, errors.New("invalid legacy timestamp")
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("invalid legacy timestamp")
}
func Read(ctx context.Context, path string) (*Snapshot, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// An offline standalone snapshot is required; immutable must not ignore a live WAL.
	for _, suffix := range []string{"-wal", "-journal"} {
		if st, e := os.Stat(absolute + suffix); e == nil && st.Size() > 0 {
			return nil, errors.New("use a consistent standalone SQLite backup; journal/WAL exists")
		}
	}
	hash, err := digest(absolute)
	if err != nil {
		return nil, err
	}
	uri := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, errors.New("cannot open SQLite")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, errors.New("cannot begin snapshot")
	}
	defer tx.Rollback()
	var integrity string
	if err = tx.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return nil, errors.New("SQLite integrity check failed")
	}
	result := &Snapshot{Report: Report{Hash: hash, InvalidFields: map[string]int{}, MissingFields: map[string]int{}}}
	read := func(name string) ([][]any, error) {
		rows, e := tx.QueryContext(ctx, query(name))
		if e != nil {
			return nil, errors.New("unsupported legacy schema: " + name)
		}
		defer rows.Close()
		columns, _ := rows.Columns()
		var values [][]any
		for rows.Next() {
			row := make([]any, len(columns))
			ptr := make([]any, len(columns))
			for i := range ptr {
				ptr[i] = &row[i]
			}
			if e = rows.Scan(ptr...); e != nil {
				return nil, errors.New("invalid legacy row")
			}
			values = append(values, row)
		}
		return values, rows.Err()
	}
	if result.Users, err = read("users"); err != nil {
		return nil, err
	}
	if result.Permissions, err = read("permissions"); err != nil {
		return nil, err
	}
	if result.Chats, err = read("chats"); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, query("message_count")).Scan(&result.Report.ArchivedMessages); err != nil {
		return nil, errors.New("cannot count archived messages")
	}
	for _, row := range result.Users {
		for _, i := range []int{5, 6} {
			if row[i], err = date(row[i]); err != nil {
				return nil, err
			}
		}
		for i, field := range domain.Fields {
			value, _ := row[9+i].(string)
			if _, e := domain.Normalize(field, value); e != nil {
				if strings.TrimSpace(value) == "" {
					result.Report.MissingFields[field]++
				} else {
					result.Report.InvalidFields[field]++
				}
			}
		}
	}
	for _, row := range result.Permissions {
		if row[4], err = date(row[4]); err != nil {
			return nil, err
		}
	}
	for _, row := range result.Chats {
		if row[5], err = date(row[5]); err != nil {
			return nil, err
		}
		switch v := row[4].(type) {
		case int64:
			row[4] = v != 0
		case bool:
		default:
			return nil, errors.New("invalid active flag")
		}
	}
	after, err := digest(absolute)
	if err != nil || after != hash {
		return nil, errors.New("snapshot changed during read")
	}
	result.Report.Users = len(result.Users)
	result.Report.Permissions = len(result.Permissions)
	result.Report.Chats = len(result.Chats)
	return result, nil
}
func Apply(ctx context.Context, pool *pgxpool.Pool, s *Snapshot) (Report, error) {
	r := s.Report
	tx, err := pool.Begin(ctx)
	if err != nil {
		return r, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, store.Q("import_lock")); err != nil {
		return r, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, store.Q("import_exists"), r.Hash).Scan(&exists); err != nil {
		return r, err
	}
	if exists {
		r.AlreadyImported = true
		return r, nil
	}
	var count int64
	if err = tx.QueryRow(ctx, store.Q("import_empty")).Scan(&count); err != nil {
		return r, err
	}
	if count != 0 {
		return r, errors.New("destination is not empty; refusing to overwrite data")
	}
	for _, batch := range []struct {
		name string
		rows [][]any
	}{{"import_user", s.Users}, {"import_permission", s.Permissions}, {"import_chat", s.Chats}} {
		for i, row := range batch.rows {
			if _, err = tx.Exec(ctx, store.Q(batch.name), row...); err != nil {
				return r, fmt.Errorf("%s row %d rejected; transaction rolled back", batch.name, i+1)
			}
		}
	}
	if _, err = tx.Exec(ctx, store.Q("reset_sequences")); err != nil {
		return r, err
	}
	if _, err = tx.Exec(ctx, store.Q("import_record"), r.Hash, r.Users); err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}
