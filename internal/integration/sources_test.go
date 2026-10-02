//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

func TestFirstStarts(t *testing.T) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	must(err)
	if cfg.ConnConfig.Database != "registration_test" {
		t.Fatal("requires disposable registration_test database")
	}
	bootstrap, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer bootstrap.Close()
	_, err = bootstrap.Exec(ctx, "CREATE SCHEMA attribution_test")
	must(err)
	cfg.ConnConfig.RuntimeParams["search_path"] = "attribution_test"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer db.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := db.Exec(ctx, sql, args...)
		must(err)
	}
	must(store.Migrate(ctx, db))
	// Reconstruct version 1 and verify the real upgrade preserves historical users.
	exec("DROP TABLE first_starts")
	exec("ALTER TABLE milestone_notifications DROP CONSTRAINT milestone_notifications_pkey")
	exec("ALTER TABLE milestone_notifications ADD COLUMN epoch BIGINT NOT NULL DEFAULT 1")
	exec("ALTER TABLE milestone_notifications ADD PRIMARY KEY(epoch,threshold)")
	exec("INSERT INTO milestone_notifications(epoch,threshold) VALUES(1,25),(2,25),(2,50)")
	exec("DELETE FROM schema_migrations WHERE version>=2")
	exec("INSERT INTO users(telegram_id) VALUES(10)")
	if store.CheckSchema(ctx, db) == nil {
		t.Fatal("old schema accepted")
	}
	must(store.Migrate(ctx, db))
	must(store.Migrate(ctx, db))
	must(store.CheckSchema(ctx, db))
	var milestones int
	must(db.QueryRow(ctx, "SELECT count(*) FROM milestone_notifications WHERE threshold IN (25,50)").Scan(&milestones))
	if milestones != 2 {
		t.Fatal("migration lost or duplicated historical milestones")
	}

	svc := &service.Service{DB: db, BotID: 1000, RootID: 1}
	seq := int64(1)
	send := func(actor int64, text string) *pb.Update {
		t.Helper()
		u := &pb.Update{Id: seq, Actor: actor, Chat: actor, ChatType: "private", Text: text}
		seq++
		_, err := svc.Accept(ctx, u)
		must(err)
		return u
	}
	check := func(actor int64, want *string) time.Time {
		t.Helper()
		var source *string
		var started *time.Time
		must(db.QueryRow(ctx, "SELECT source,started_at FROM first_starts WHERE telegram_id=$1", actor).Scan(&source, &started))
		if want == nil {
			if source != nil || started != nil {
				t.Fatal("historical user attributed to a new link")
			}
			return time.Time{}
		}
		if source == nil || *source != *want || started == nil {
			t.Fatalf("unexpected attribution for %d", actor)
		}
		return *started
	}
	tag, direct := "website", ""
	send(10, "/start website")
	check(10, nil)
	first := send(20, "/start website")
	at := check(20, &tag)
	receipt, err := svc.Accept(ctx, first)
	must(err)
	if !receipt.Duplicate {
		t.Fatal("replayed update not deduplicated")
	}
	send(20, "/start other")
	send(20, "/start")
	if !check(20, &tag).Equal(at) {
		t.Fatal("first timestamp overwritten")
	}
	var name *string
	var state string
	must(db.QueryRow(ctx, "SELECT name,state FROM users WHERE telegram_id=20").Scan(&name, &state))
	if name != nil || state != "name" {
		t.Fatal("deep link was treated as a survey answer")
	}
	send(21, "/start")
	send(21, "/start website")
	check(21, &direct)
	send(22, "/start invalid.payload")
	send(22, "/start website")
	check(22, &direct)
	send(23, "/start@fixture_bot website")
	send(23, "/start other")
	check(23, &tag)
	_, err = svc.Accept(ctx, &pb.Update{Id: seq, Actor: 24, Chat: -100, ChatType: "supergroup", Text: "/start website"})
	seq++
	must(err)
	var n int
	must(db.QueryRow(ctx, "SELECT count(*) FROM first_starts WHERE telegram_id=24").Scan(&n))
	if n != 0 {
		t.Fatal("group start counted")
	}
	// A failed outbox write must roll back the source together with the update.
	exec("ALTER TABLE outbound_messages ADD CONSTRAINT fixture_failure CHECK(chat<>25)")
	failed := &pb.Update{Id: seq, Actor: 25, Chat: 25, ChatType: "private", Text: "/start website"}
	seq++
	if _, err = svc.Accept(ctx, failed); err == nil {
		t.Fatal("expected fixture persistence failure")
	}
	must(db.QueryRow(ctx, "SELECT count(*) FROM first_starts WHERE telegram_id=25").Scan(&n))
	if n != 0 {
		t.Fatal("attribution committed without outbox")
	}
	exec("ALTER TABLE outbound_messages DROP CONSTRAINT fixture_failure")
	failed.Text = "/start recovered"
	_, err = svc.Accept(ctx, failed)
	must(err)
	recovered := "recovered"
	check(25, &recovered)
	// Concurrent starts serialize on the user and cannot create extra counts.
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			_, err := svc.Accept(ctx, &pb.Update{Id: int64(1000 + i), Actor: 30, Chat: 30, ChatType: "private", Text: fmt.Sprintf("/start race_%d", i)})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(err)
	}
	must(db.QueryRow(ctx, "SELECT count(*) FROM first_starts WHERE telegram_id=30").Scan(&n))
	if n != 1 {
		t.Fatal("concurrent starts double-counted")
	}
	view := func(actor int64, command string) *pb.View {
		t.Helper()
		send(actor, command)
		var data []byte
		must(db.QueryRow(ctx, "SELECT body FROM outbound_messages WHERE chat=$1 ORDER BY id DESC LIMIT 1", actor).Scan(&data))
		v := &pb.View{}
		must(protojson.Unmarshal(data, v))
		return v
	}
	if v := view(20, "/sources"); v.Code != "denied" {
		t.Fatal("unauthorized statistics", v)
	}
	v := view(1, "/sources")
	counts := map[string]string{}
	for _, f := range v.Fields {
		counts[f.Key] = f.Value
	}
	if v.Kind != "sources" || counts[tag] != "2" || counts[""] != "2" || counts[":unknown"] != "1" || len(counts) != 5 {
		t.Fatal("wrong source counts", v)
	}
	for i := range 22 {
		send(int64(100+i), fmt.Sprintf("/start campaign_%02d", i))
	}
	v = view(1, "/sources")
	if len(v.Fields) != 20 || len(v.Numbers) != 2 || v.Numbers[1] != 2 {
		t.Fatal("missing next page", v)
	}
	seen := map[string]bool{}
	for _, f := range v.Fields {
		seen[f.Key] = true
	}
	v = view(1, "/sources 2")
	if len(v.Fields) != 7 || len(v.Numbers) != 1 {
		t.Fatal("wrong last page", v)
	}
	for _, f := range v.Fields {
		if seen[f.Key] {
			t.Fatal("duplicate source across pages")
		}
	}
	if v = view(1, "/sources 0"); v.Code != "sources_usage" {
		t.Fatal("invalid page accepted")
	}
	exported, err := svc.Export(ctx, &pb.ExportRequest{Actor: 1})
	must(err)
	file, err := excelize.OpenReader(bytes.NewReader(exported.Xlsx))
	must(err)
	defer file.Close()
	rows, err := file.GetRows("Sheet1")
	must(err)
	if rows[0][12] != "first_start_source" {
		t.Fatal("missing export source header")
	}
	for _, row := range rows[1:] {
		if row[0] == "20" && (row[12] != tag || row[13] != "tagged" || len(row) != 15) {
			t.Fatal("wrong exported source", row)
		}
	}
}
