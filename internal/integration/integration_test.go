//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"github.com/xuri/excelize/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/app"
	"registration.local/backend/internal/legacy"
	"registration.local/backend/internal/queue"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

//go:embed fixture.sql
var fixture string

func TestSystem(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "registration_test" {
		t.Fatal("requires disposable registration_test database")
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...any) { t.Helper(); _, err := db.Exec(ctx, query, args...); must(err) }
	must(store.Migrate(ctx, db))
	must(store.Migrate(ctx, db))
	must(store.CheckSchema(ctx, db))
	path := filepath.Join(t.TempDir(), "fixture.sqlite")
	sqlite, err := sql.Open("sqlite", path)
	must(err)
	_, err = sqlite.Exec(fixture)
	must(err)
	must(sqlite.Close())
	snapshot, err := legacy.Read(ctx, path)
	must(err)
	if snapshot.Report.Users != 1 || snapshot.Report.ArchivedMessages != 1 {
		t.Fatal("snapshot counts")
	}
	report, err := legacy.Apply(ctx, db, snapshot)
	must(err)
	if report.AlreadyImported {
		t.Fatal("first import skipped")
	}
	var state string
	var phone *string
	must(db.QueryRow(ctx, "SELECT state,phone FROM users WHERE telegram_id=7000000001").Scan(&state, &phone))
	if state != "new" || phone != nil {
		t.Fatal("import should reset state, preserve NULL")
	}
	exec("UPDATE users SET name='Manually updated' WHERE telegram_id=7000000001")
	report, err = legacy.Apply(ctx, db, snapshot)
	must(err)
	if !report.AlreadyImported {
		t.Fatal("repeat not recognized")
	}
	var name string
	must(db.QueryRow(ctx, "SELECT name FROM users WHERE telegram_id=7000000001").Scan(&name))
	if name != "Manually updated" {
		t.Fatal("manual edit overwritten")
	}
	var orphan int
	must(db.QueryRow(ctx, "SELECT count(*) FROM user_permissions WHERE telegram_id=7000000099").Scan(&orphan))
	if orphan != 1 {
		t.Fatal("orphan permission lost")
	}
	snapshot.Report.Hash = "different"
	if _, err = legacy.Apply(ctx, db, snapshot); err == nil {
		t.Fatal("nonempty destination overwritten")
	}
	svc := &service.Service{DB: db, BotID: 1000, RootID: 101, Milestones: []int{1}}
	// Exercise the actual protobuf and authentication boundary, not just direct method calls.
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(app.Auth(strings.Repeat("x", 32))))
	pb.RegisterRegistrationServer(server, svc)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	must(err)
	defer conn.Close()
	client := pb.NewRegistrationClient(conn)
	_, err = client.Claim(ctx, &pb.ClaimRequest{Worker: "fixture-worker"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("missing auth accepted")
	}
	auth := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.Repeat("x", 32))
	user := int64(7000000001)
	seq := int64(1)
	send := func(actor int64, text, callback string) {
		t.Helper()
		_, err := client.Accept(auth, &pb.Update{Id: seq, Actor: actor, Chat: actor, ChatType: "private", Text: text, Callback: callback})
		seq++
		must(err)
	}
	send(user, "/start", "")
	must(db.QueryRow(ctx, "SELECT state FROM users WHERE telegram_id=$1", user).Scan(&state))
	if state != "phone" {
		t.Fatal("filled answers were not skipped", state)
	}
	before := seq - 1
	_, err = client.Accept(auth, &pb.Update{Id: before, Actor: user, Chat: user, ChatType: "private", Text: "overwrite"})
	must(err)
	send(user, "+7 (999) 123-45-67", "")
	var version int64
	must(db.QueryRow(ctx, "SELECT state,version FROM users WHERE telegram_id=$1", user).Scan(&state, &version))
	if state != "confirm" {
		t.Fatal("expected confirmation", state)
	}
	send(user, "", "select|old")
	send(user, "", "c:3:2:confirm") // Old epoch-bearing format is stale.
	send(user, "", "c:1:confirm")   // Current format, stale user version.
	must(db.QueryRow(ctx, "SELECT version FROM users WHERE telegram_id=$1", user).Scan(&version))
	if version != 2 {
		t.Fatal("legacy callback changed state")
	}
	send(user, "", "c:2:confirm")
	must(db.QueryRow(ctx, "SELECT state FROM users WHERE telegram_id=$1", user).Scan(&state))
	if state != "registered" {
		t.Fatal("not registered")
	}
	send(user, "/export", "")
	var count int
	must(db.QueryRow(ctx, "SELECT count(*) FROM outbound_messages WHERE kind='export'").Scan(&count))
	if count != 0 {
		t.Fatal("unauthorized export")
	}
	export, err := client.Export(auth, &pb.ExportRequest{Actor: 7000000099})
	must(err)
	file, err := excelize.OpenReader(bytes.NewReader(export.Xlsx))
	must(err)
	defer file.Close()
	cell, err := file.GetCellValue("Sheet1", "E2")
	must(err)
	if cell != "Manually updated" {
		t.Fatal("wrong XLSX content")
	}
	send(user, "", "p:3:1") // Old poll callback must not change the answer.
	must(db.QueryRow(ctx, "SELECT version FROM users WHERE telegram_id=$1", user).Scan(&version))
	if version != 3 {
		t.Fatal("old poll callback changed state")
	}
	send(user, "", "p:1")
	var attendance string
	must(db.QueryRow(ctx, "SELECT trip_attendance FROM users WHERE telegram_id=$1", user).Scan(&attendance))
	if attendance != "Нет, не смогу 😢" {
		t.Fatal("new poll callback was not accepted")
	}
	// Recipient set is frozen at send; replaying launch does not duplicate deliveries.
	send(101, "/poll registered", "")
	send(101, "/send 1", "")
	var deliveries int
	must(db.QueryRow(ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=1").Scan(&deliveries))
	if deliveries != 1 {
		t.Fatal("audience selection", deliveries)
	}
	send(101, "/send 1", "")
	must(db.QueryRow(ctx, "SELECT count(*) FROM outbound_messages WHERE broadcast_id=1").Scan(&count))
	if count != deliveries {
		t.Fatal("launch duplicated")
	}
	exec("UPDATE outbound_messages SET status='sent' WHERE broadcast_id IS NULL")
	var job int64
	must(db.QueryRow(ctx, "SELECT id FROM outbound_messages WHERE broadcast_id=1").Scan(&job))
	claim, err := client.Claim(auth, &pb.ClaimRequest{Id: job, Worker: "fixture-worker"})
	must(err)
	if claim.Id != job {
		t.Fatal("claim failed")
	}
	_, err = client.Claim(auth, &pb.ClaimRequest{Id: job, Worker: "other-worker"})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatal("second sender accepted")
	}
	duplicate, err := client.Claim(auth, &pb.ClaimRequest{Id: job, Worker: "fixture-worker"})
	must(err)
	if duplicate.Id != 0 {
		t.Fatal("duplicate lease")
	}
	_, err = client.Complete(auth, &pb.Completion{Id: job, Lease: claim.Lease, Outcome: "rate_limit", RetryAfterSeconds: 30})
	must(err)
	var attempts int
	must(db.QueryRow(ctx, "SELECT status,attempts FROM outbound_messages WHERE id=$1", job).Scan(&state, &attempts))
	if state != "retry_wait" || attempts != 0 {
		t.Fatal("rate limit spent failure budget")
	}
	cooled, err := client.Claim(auth, &pb.ClaimRequest{Id: job, Worker: "fixture-worker"})
	must(err)
	if cooled.Id != 0 || cooled.NotBeforeUnixMs <= time.Now().UnixMilli() {
		t.Fatal("cooldown not persisted")
	}
	exec("UPDATE runtime_state SET cooldown_until=now()-interval '1 second'")
	exec("UPDATE outbound_messages SET next_attempt_at=now()-interval '1 second' WHERE id=$1", job)
	next, err := client.Claim(auth, &pb.ClaimRequest{Id: job, Worker: "fixture-worker"})
	must(err)
	_, err = client.Complete(auth, &pb.Completion{Id: job, Lease: claim.Lease, Outcome: "sent"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatal("stale worker accepted")
	}
	exec("UPDATE outbound_messages SET lease_until=now()-interval '1 second' WHERE id=$1", job)
	recovered, err := client.Claim(auth, &pb.ClaimRequest{Id: job, Worker: "fixture-worker"})
	must(err)
	if recovered.Id == 0 || recovered.Lease == next.Lease {
		t.Fatal("expired lease not reclaimed")
	}
	_, err = client.Complete(auth, &pb.Completion{Id: job, Lease: recovered.Lease, Outcome: "sent", TelegramMessageId: 17})
	must(err)
	duplicate, err = client.Claim(auth, &pb.ClaimRequest{Id: job, Worker: "fixture-worker"})
	must(err)
	if duplicate.Id != 0 {
		t.Fatal("sent job reclaimed")
	}
	send(101, "/poll registered", "")
	send(101, "/send 2", "")
	send(101, "/pause 2", "")
	must(db.QueryRow(ctx, "SELECT id FROM outbound_messages WHERE broadcast_id=2").Scan(&job))
	claim, err = client.Claim(auth, &pb.ClaimRequest{Id: job, Worker: "fixture-worker"})
	must(err)
	if claim.Id != 0 {
		t.Fatal("paused job claimed")
	}
	send(101, "/cancel_broadcast 2", "")
	must(db.QueryRow(ctx, "SELECT status FROM outbound_messages WHERE id=$1", job).Scan(&state))
	if state != "cancelled" {
		t.Fatal("cancel failed")
	}
	// Real Kafka transport, with disposable broker and no live topics.
	broker := os.Getenv("TEST_KAFKA_BROKER")
	if broker == "" {
		t.Fatal("fixture Kafka required")
	}
	t.Setenv("KAFKA_BROKERS", broker)
	t.Setenv("KAFKA_TOPIC_PREFIX", "registration.fixture")
	must(app.InitTopics(ctx))
	exec("UPDATE outbound_messages SET status='sent' WHERE status<>'cancelled'")
	send(user, "/start", "")
	must(db.QueryRow(ctx, "SELECT max(id) FROM outbound_messages WHERE priority='interactive' AND status='pending'").Scan(&job))
	writer := &kafka.Writer{Addr: kafka.TCP(broker), Balancer: &kafka.Hash{}, RequiredAcks: kafka.RequireAll, WriteTimeout: 5 * time.Second}
	defer writer.Close()
	publisher := &queue.Publisher{DB: db, Writer: writer, Prefix: "registration.fixture"}
	call, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	must(publisher.Once(call))
	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{broker}, Topic: "registration.fixture.interactive.v1", GroupID: "fixture", MinBytes: 1, MaxBytes: 65536, StartOffset: kafka.FirstOffset})
	defer reader.Close()
	message, err := reader.FetchMessage(call)
	must(err)
	if string(message.Value) != strconv.FormatInt(job, 10) {
		t.Fatal("wrong queue identifier")
	}
	if bytes.Contains(message.Value, []byte("Manually")) {
		t.Fatal("personal data in Kafka payload")
	}
	must(reader.CommitMessages(call, message))
	// Simulate a lost Kafka event after commit: stale publication is discoverable from PostgreSQL.
	exec("UPDATE outbound_messages SET published_at=now()-interval '31 seconds' WHERE id=$1", job)
	must(publisher.Once(call))
	replayed, err := reader.FetchMessage(call)
	must(err)
	if !bytes.Equal(message.Value, replayed.Value) {
		t.Fatal("outbox did not recover lost event")
	}
}
