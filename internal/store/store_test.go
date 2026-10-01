package store

import (
	"slices"
	"testing"
	"testing/fstest"
)

func TestMigrationDiscovery(t *testing.T) {
	source := fstest.MapFS{
		"migrations/1_initial.sql": {},
		"migrations/02_next.sql":   {},
		"migrations/003_last.sql":  {},
		"migrations/README.md":     {},
	}
	want := []string{"migrations/1_initial.sql", "migrations/02_next.sql", "migrations/003_last.sql"}
	got, err := migrationPaths(source)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("numeric migration order: got %v, error %v", got, err)
	}
	// A new file is picked up without registration in application code.
	source["migrations/004_added.sql"] = &fstest.MapFile{}
	want = append(want, "migrations/004_added.sql")
	got, err = migrationPaths(source)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("new migration not discovered: got %v, error %v", got, err)
	}
}

func TestInvalidMigrationSequences(t *testing.T) {
	for name, paths := range map[string][]string{
		"empty":       {},
		"missing one": {"002_next.sql"},
		"gap":         {"001_first.sql", "003_third.sql"},
		"duplicate":   {"001_first.sql", "01_other.sql"},
		"zero":        {"000_zero.sql"},
		"no number":   {"initial.sql"},
		"signed":      {"+1_initial.sql"},
		"no name":     {"001_.sql"},
		"overflow":    {"2147483648_large.sql"},
	} {
		t.Run(name, func(t *testing.T) {
			source := fstest.MapFS{}
			for _, file := range paths {
				source["migrations/"+file] = &fstest.MapFile{}
			}
			if _, err := migrationPaths(source); err == nil {
				t.Fatal("invalid migration sequence accepted")
			}
		})
	}
}
