package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"registration.local/backend/internal/domain"
	"registration.local/backend/internal/legacy"
	"registration.local/backend/internal/store"
)

func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: registration serve|migrate|import-sqlite|validate-data")
	}
	if args[0] == "init-topics" {
		return InitTopics(ctx)
	}
	if args[0] == "serve" {
		c, err := Load()
		if err != nil {
			return err
		}
		return Serve(ctx, c)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if args[0] == "import-sqlite" {
		fs := flag.NewFlagSet("import-sqlite", flag.ContinueOnError)
		source := fs.String("source", "", "standalone SQLite snapshot")
		dry := fs.Bool("dry-run", false, "inspect only")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *source == "" {
			return errors.New("--source is required")
		}
		snapshot, err := legacy.Read(bounded, *source)
		if err != nil {
			return err
		}
		if *dry {
			return json.NewEncoder(os.Stdout).Encode(snapshot.Report)
		}
		db, err := store.Open(bounded, os.Getenv("DATABASE_URL"))
		if err != nil {
			return err
		}
		defer db.Close()
		if err = store.CheckSchema(bounded, db); err != nil {
			return err
		}
		report, err := legacy.Apply(bounded, db, snapshot)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	if args[0] != "migrate" && args[0] != "validate-data" {
		return errors.New("unknown command")
	}
	db, err := store.Open(bounded, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	if args[0] == "migrate" {
		if err = store.Migrate(bounded, db); err != nil {
			return errors.New("migration failed; inspect schema compatibility")
		}
		fmt.Println("schema ready")
		return nil
	}
	if err = store.CheckSchema(bounded, db); err != nil {
		return err
	}
	rows, err := db.Query(bounded, store.Q("validate"))
	if err != nil {
		return errors.New("validation query failed")
	}
	defer rows.Close()
	counts := map[string]int{}
	total := 0
	for rows.Next() {
		values := make([]string, len(domain.Fields))
		ptr := make([]any, len(values))
		for i := range ptr {
			ptr[i] = &values[i]
		}
		if err = rows.Scan(ptr...); err != nil {
			return errors.New("invalid stored data")
		}
		total++
		for i, f := range domain.Fields {
			if _, e := domain.Normalize(f, values[i]); e != nil {
				kind := "invalid:"
				if values[i] == "" {
					kind = "missing:"
				}
				counts[kind+f]++
			}
		}
	}
	if err = rows.Err(); err != nil {
		return errors.New("validation interrupted")
	}
	if err = json.NewEncoder(os.Stdout).Encode(struct {
		Users  int
		Issues map[string]int
	}{total, counts}); err != nil {
		return err
	}
	for key := range counts {
		if len(key) >= 8 && key[:8] == "invalid:" {
			return errors.New("invalid nonempty answers require review")
		}
	}
	return nil
}
