//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/app"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

func TestDirectRecovery(t *testing.T) {
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
	_, err = bootstrap.Exec(ctx, "CREATE SCHEMA direct_test")
	must(err)
	cfg.ConnConfig.RuntimeParams["search_path"] = "direct_test"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer db.Close()
	exec := func(sql string, args ...any) { t.Helper(); _, err := db.Exec(ctx, sql, args...); must(err) }
	must(store.Migrate(ctx, db))
	// Reconstruct schema v3 and exercise the upgrade with outstanding jobs.
	exec("ALTER TABLE outbound_messages DROP COLUMN update_bot_id CASCADE, DROP COLUMN update_id CASCADE, DROP COLUMN edit_message_id CASCADE")
	exec("DROP INDEX outbound_interactive_due")
	exec("DELETE FROM schema_migrations WHERE version=4")
	exec(`INSERT INTO outbound_messages(chat,priority,kind,body,status,attempts) VALUES
	 (90,'interactive','view','{"kind":"help"}','retry_wait',2),
	 (91,'interactive','view','{"kind":"permissions","fields":[{"key":"permission","value":"admin"}]}','pending',0),
	 (92,'interactive','view','{"kind":"help"}','sent',0)`)
	must(store.Migrate(ctx, db))
	must(store.CheckSchema(ctx, db))
	var legacyKind, legacyCode, legacyStatus string
	var legacyAttempts int
	must(db.QueryRow(ctx, "SELECT body->>'kind',status,attempts FROM outbound_messages WHERE chat=90").Scan(&legacyKind, &legacyStatus, &legacyAttempts))
	if legacyKind != "help_participant" || legacyStatus != "retry_wait" || legacyAttempts != 2 {
		t.Fatal("legacy reply upgrade lost safety or retry state")
	}
	must(db.QueryRow(ctx, "SELECT body->>'code' FROM outbound_messages WHERE chat=91").Scan(&legacyCode))
	if legacyCode != "unavailable" {
		t.Fatal("legacy permissions exposed")
	}
	must(db.QueryRow(ctx, "SELECT body->>'kind' FROM outbound_messages WHERE chat=92").Scan(&legacyKind))
	if legacyKind != "help" {
		t.Fatal("sent history rewritten")
	}
	svc := &service.Service{DB: db, BotID: 1000, RootID: 1, Milestones: []int{1}, ParticipationEnabled: true}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(app.Auth(strings.Repeat("x", 32))))
	pb.RegisterRegistrationServer(server, svc)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///direct", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	must(err)
	defer conn.Close()
	client := pb.NewRegistrationClient(conn)
	_, err = client.PendingInteractive(ctx, &pb.PendingInteractiveRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("recovery must require authentication")
	}
	auth := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.Repeat("x", 32))
	for _, limit := range []int32{-1, 101} {
		_, err = client.PendingInteractive(auth, &pb.PendingInteractiveRequest{Limit: limit})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatal("unbounded recovery accepted")
		}
	}
	seq := int64(1)
	send := func(actor int64, text string) (*pb.Update, *pb.Receipt) {
		t.Helper()
		u := &pb.Update{Id: seq, Actor: actor, Chat: actor, ChatType: "private", Text: text}
		seq++
		r, err := client.Accept(auth, u)
		must(err)
		if len(r.DeliveryIds) == 0 {
			t.Fatal("missing direct reply")
		}
		return u, r
	}
	pending := func(limit int32) []int64 {
		t.Helper()
		r, err := client.PendingInteractive(auth, &pb.PendingInteractiveRequest{Limit: limit})
		must(err)
		return r.DeliveryIds
	}
	claim := func(id int64) *pb.Delivery {
		t.Helper()
		d, err := client.Claim(auth, &pb.ClaimRequest{Id: id, Worker: "direct-worker"})
		must(err)
		return d
	}
	complete := func(d *pb.Delivery, outcome string) {
		t.Helper()
		_, err := client.Complete(auth, &pb.Completion{Id: d.Id, Lease: d.Lease, Outcome: outcome, RetryAfterSeconds: 30})
		must(err)
	}
	legacyIDs := pending(0)
	if len(legacyIDs) != 2 {
		t.Fatal("legacy jobs not discoverable")
	}
	for _, id := range legacyIDs {
		d := claim(id)
		if d.Id != 0 {
			if d.View.Kind != "help_public" {
				t.Fatal("legacy reply exposed operator content")
			}
			complete(d, "sent")
		}
	}

	// Simulate a lost Accept response: replay returns the original durable IDs,
	// including concurrently, without repeating mutation or creating jobs.
	u, first := send(10, "/start")
	if len(first.DeliveryIds) != 2 {
		t.Fatal("start must persist greeting and first question")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, err := client.Accept(auth, u)
			if err != nil || !r.GetDuplicate() || !slices.Equal(r.GetDeliveryIds(), first.DeliveryIds) {
				t.Error("unstable duplicate replay")
			}
		})
	}
	wg.Wait()
	var version int64
	must(db.QueryRow(ctx, "SELECT version FROM users WHERE telegram_id=10").Scan(&version))
	if version != 1 {
		t.Fatal("duplicate changed state")
	}
	_, second := send(10, "/about")
	_, other := send(11, "/start")
	ids := pending(0)
	if !slices.Equal(ids, []int64{first.DeliveryIds[0], other.DeliveryIds[0]}) {
		t.Fatalf("head-of-chat recovery: %v", ids)
	}
	if len(pending(1)) != 1 {
		t.Fatal("limit ignored")
	}
	if claim(second.DeliveryIds[0]).Id != 0 {
		t.Fatal("reply order bypassed")
	}
	d := claim(first.DeliveryIds[0])
	if d.View.Kind != "welcome" || len(d.View.Buttons) != 0 {
		t.Fatal("expected greeting without menu")
	}
	if slices.Contains(pending(0), d.Id) {
		t.Fatal("active lease rediscovered")
	}
	complete(d, "rate_limit")
	if len(pending(0)) != 0 {
		t.Fatal("global cooldown ignored")
	}
	exec("UPDATE runtime_state SET cooldown_until=now()-interval '1 second'")
	if slices.Contains(pending(0), d.Id) {
		t.Fatal("retry scheduled too early")
	}
	exec("UPDATE outbound_messages SET next_attempt_at=now()-interval '1 second' WHERE id=$1", d.Id)
	if !slices.Contains(pending(0), d.Id) {
		t.Fatal("retry not recovered")
	}
	d = claim(d.Id)
	old := d.Lease
	// Simulate process death after Claim. A new lease fences the old worker.
	exec("UPDATE outbound_messages SET lease_until=now()-interval '1 second' WHERE id=$1", d.Id)
	if !slices.Contains(pending(0), d.Id) {
		t.Fatal("expired lease not recovered")
	}
	d = claim(d.Id)
	_, err = client.Complete(auth, &pb.Completion{Id: d.Id, Lease: old, Outcome: "sent"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatal("old lease accepted")
	}
	complete(d, "sent")
	if claim(d.Id).Id != 0 {
		t.Fatal("sent response resent")
	}
	r, err := client.Accept(auth, u)
	must(err)
	if !slices.Equal(r.DeliveryIds, first.DeliveryIds) {
		t.Fatal("completed replay changed IDs")
	}
	d = claim(first.DeliveryIds[1])
	if d.Id == 0 || d.View.Kind != "question" || d.View.Field != "name" {
		t.Fatal("greeting must be followed by name question without a callback")
	}
	complete(d, "sent")
	if !slices.Contains(pending(0), second.DeliveryIds[0]) {
		t.Fatal("next reply not unblocked")
	}
	exec("UPDATE outbound_messages SET status='sent'")

	// Flexible date persists canonically through the real gRPC transaction.
	exec("UPDATE users SET state='birth_date' WHERE telegram_id=10")
	send(10, "10.3.2002")
	var date string
	must(db.QueryRow(ctx, "SELECT birth_date FROM users WHERE telegram_id=10").Scan(&date))
	if date != "10.03.2002" {
		t.Fatal("date not normalized")
	}
	exec("UPDATE outbound_messages SET status='sent'")
	exec(`UPDATE users SET state='confirm',name='Fixture User',"group"='FIXTURE',phone='79991234567',expectations='Fixture',will_drive='Обязательно! 🤩',trip_attendance='Да, точно еду! ✅' WHERE telegram_id=10`)
	must(db.QueryRow(ctx, "SELECT version FROM users WHERE telegram_id=10").Scan(&version))
	callback := &pb.Update{Id: seq, Actor: 10, Chat: 10, ChatType: "private", Callback: "c:" + strconv.FormatInt(version, 10) + ":edit", MessageId: 42, CallbackMessageEditable: true}
	seq++
	r, err = client.Accept(auth, callback)
	must(err)
	d = claim(r.DeliveryIds[0])
	if d.EditMessageId != 42 || d.View.Kind != "edit" {
		t.Fatal("callback edit target lost")
	}
	complete(d, "sent")
	var phoneAction string
	for _, b := range d.View.Buttons {
		if b.LabelKey == "phone" {
			phoneAction = b.Data
		}
	}
	if phoneAction == "" {
		t.Fatal("missing phone edit button")
	}
	phoneUpdate := &pb.Update{Id: seq, Actor: 10, Chat: 10, ChatType: "private", Callback: phoneAction, MessageId: 42, CallbackMessageEditable: true}
	seq++
	r, err = client.Accept(auth, phoneUpdate)
	must(err)
	if len(r.DeliveryIds) != 1 {
		t.Fatal("phone edit must persist a single question with its reply keyboard")
	}
	d = claim(r.DeliveryIds[0])
	if d.EditMessageId != 0 || d.View.Kind != "question" || d.View.Field != "phone" || len(d.View.Buttons) != 1 || d.View.Buttons[0].LabelKey != "cancel" {
		t.Fatal("phone question must be a new message with reply cancellation")
	}
	complete(d, "sent")
	replay, err := client.Accept(auth, phoneUpdate)
	must(err)
	if !replay.Duplicate || !slices.Equal(replay.DeliveryIds, r.DeliveryIds) {
		t.Fatal("phone edit replay changed deliveries")
	}
	r, err = client.Accept(auth, &pb.Update{Id: seq, Actor: 10, Chat: 10, ChatType: "private", Kind: "message", Phone: "79991234568", ContactOwner: 11})
	seq++
	must(err)
	if len(r.DeliveryIds) != 1 {
		t.Fatal("foreign contact must produce only one validation message")
	}
	d = claim(r.DeliveryIds[0])
	if d.View.Kind != "validation" || d.View.Field != "phone" || d.View.Code != "own_contact" || len(d.View.Buttons) != 1 || d.View.Buttons[0].LabelKey != "cancel" {
		t.Fatal("foreign contact must retain phone keyboard and cancellation")
	}
	complete(d, "sent")
	_, r = send(10, "invalid phone")
	if len(r.DeliveryIds) != 1 {
		t.Fatal("validation must not send a separate contact prompt")
	}
	d = claim(r.DeliveryIds[0])
	if d.View.Kind != "validation" || len(d.View.Buttons) != 1 || d.View.Buttons[0].LabelKey != "cancel" {
		t.Fatal("phone validation lost reply cancellation button")
	}
	for _, id := range r.DeliveryIds {
		if id != d.Id {
			d = claim(id)
		}
		complete(d, "sent")
	}
	// Frontend maps the reply button label to the existing cancel command.
	cancelUpdate := &pb.Update{Id: seq, Actor: 10, Chat: 10, ChatType: "private", Kind: "message", Text: "/cancel", MessageId: 43}
	r, err = client.Accept(auth, cancelUpdate)
	seq++
	must(err)
	if len(r.DeliveryIds) != 2 {
		t.Fatal("phone cancellation must persist form and keyboard removal")
	}
	d = claim(r.DeliveryIds[0])
	if d.EditMessageId != 0 || d.View.Kind != "confirm" || len(d.View.Buttons) == 0 {
		t.Fatal("reply cancellation did not leave phone editing")
	}
	complete(d, "sent")
	d = claim(r.DeliveryIds[1])
	if d.EditMessageId != 0 || d.View.Kind != "notice" || d.View.Code != "edit_cancelled" || len(d.View.Buttons) != 0 {
		t.Fatal("phone cancellation must send a keyboard-removal notice")
	}
	complete(d, "sent")
	replay, err = client.Accept(auth, cancelUpdate)
	must(err)
	if !replay.Duplicate || !slices.Equal(replay.DeliveryIds, r.DeliveryIds) {
		t.Fatal("phone cancellation replay changed deliveries")
	}
	var phoneAfter string
	must(db.QueryRow(ctx, "SELECT phone FROM users WHERE telegram_id=10").Scan(&phoneAfter))
	if phoneAfter != "79991234567" {
		t.Fatal("cancellation changed the saved phone")
	}
	var stateBefore, stateAfter string
	must(db.QueryRow(ctx, "SELECT state,version FROM users WHERE telegram_id=10").Scan(&stateBefore, &version))
	for _, action := range []string{"info:about", "info:bring"} {
		r, err = client.Accept(auth, &pb.Update{Id: seq, Actor: 10, Chat: 10, ChatType: "private", Callback: action, MessageId: 44, CallbackMessageEditable: true})
		seq++
		must(err)
		d = claim(r.DeliveryIds[0])
		if d.EditMessageId != 44 || "info:"+d.View.Kind != action {
			t.Fatal("information menu did not edit message")
		}
		complete(d, "sent")
	}
	var versionAfter int64
	must(db.QueryRow(ctx, "SELECT state,version FROM users WHERE telegram_id=10").Scan(&stateAfter, &versionAfter))
	if stateAfter != stateBefore || versionAfter != version {
		t.Fatal("information menu changed questionnaire state")
	}

	// Inaccessible and unknown commands commit dedup, but no outbox effects.
	silent := func(actor int64, chat int64, chatType, command string) {
		t.Helper()
		var before, after int
		must(db.QueryRow(ctx, "SELECT count(*) FROM outbound_messages").Scan(&before))
		u := &pb.Update{Id: seq, Actor: actor, Chat: chat, ChatType: chatType, Text: command}
		seq++
		r, err := client.Accept(auth, u)
		must(err)
		if r.Duplicate || len(r.DeliveryIds) != 0 {
			t.Fatalf("inaccessible command produced a reply: %s", command)
		}
		r, err = client.Accept(auth, u)
		must(err)
		if !r.Duplicate || len(r.DeliveryIds) != 0 {
			t.Fatal("silent command replay changed")
		}
		must(db.QueryRow(ctx, "SELECT count(*) FROM outbound_messages").Scan(&after))
		if before != after {
			t.Fatal("silent command created outbox effects")
		}
	}
	commands := []string{"/my_permissions", "/stats", "/sources invalid", "/export", "/grant_permission 99 admin", "/revoke_permission", "/register_staff_chat", "/register_counselor_chat", "/register_superuser_chat", "/sync_staff_chat", "/sync_counselor_chat", "/broadcast staff", "/poll staff", "/send 1", "/progress 1", "/pause 1", "/resume 1", "/cancel_broadcast 1", "/retry 1", "/unknown"}
	for _, command := range commands {
		silent(12, 12, "private", command)
		silent(12, -100, "supergroup", command)
	}
	_, r = send(12, "/help")
	d = claim(r.DeliveryIds[0])
	if d.View.Kind != "help_public" || len(d.View.Fields) != 0 {
		t.Fatal("operator help exposed")
	}
	complete(d, "sent")
	// Role membership alone is not operator permission.
	exec("INSERT INTO user_permissions(telegram_id,permission) VALUES(12,'staff')")
	_, r = send(12, "/help")
	d = claim(r.DeliveryIds[0])
	if d.View.Kind != "help_public" {
		t.Fatal("staff membership exposed operator help")
	}
	complete(d, "sent")
	for _, command := range commands {
		silent(12, 12, "private", command)
	}
	// Having one operator permission does not permit unrelated commands.
	exec("INSERT INTO user_permissions(telegram_id,permission) VALUES(12,'table_viewer')")
	silent(12, 12, "private", "/grant_permission 99 admin")
	silent(12, 12, "private", "/broadcast staff")
	_, r = send(12, "/sources invalid")
	d = claim(r.DeliveryIds[0])
	if d.View.Code != "sources_usage" {
		t.Fatal("authorized command validation suppressed")
	}
	complete(d, "sent")
	_, r = send(1, "/help")
	d = claim(r.DeliveryIds[0])
	if d.View.Kind != "help" {
		t.Fatal("operator help unavailable")
	}
	complete(d, "sent")

	for _, command := range []string{"/help", "/my_permissions"} {
		if command == "/my_permissions" {
			silent(1, -100, "supergroup", command)
			continue
		}
		group := &pb.Update{Id: seq, Actor: 1, Chat: -100, ChatType: "supergroup", Text: command}
		seq++
		r, err := client.Accept(auth, group)
		must(err)
		d := claim(r.DeliveryIds[0])
		if d.View.Kind != "help_public" {
			t.Fatal("group exposed operator help")
		}
		complete(d, "sent")
	}

	// Delayed operator views cannot outlive access.
	exec("INSERT INTO user_permissions(telegram_id,permission) VALUES(13,'admin')")
	_, help := send(13, "/help")
	_, permissions := send(13, "/my_permissions")
	exec("DELETE FROM user_permissions WHERE telegram_id=13")
	d = claim(help.DeliveryIds[0])
	if d.Id != 0 || d.View != nil {
		t.Fatal("queued help outlived access")
	}
	d = claim(permissions.DeliveryIds[0])
	if d.Id != 0 || d.View != nil {
		t.Fatal("queued permissions outlived access")
	}
	for _, id := range []int64{help.DeliveryIds[0], permissions.DeliveryIds[0]} {
		var state string
		must(db.QueryRow(ctx, "SELECT status FROM outbound_messages WHERE id=$1", id).Scan(&state))
		if state != "cancelled" || claim(id).Id != 0 || slices.Contains(pending(0), id) {
			t.Fatal("suppressed reply not terminal")
		}
	}
	_, r = send(13, "/about")
	d = claim(r.DeliveryIds[0])
	if d.View.Kind != "about" {
		t.Fatal("cancelled admin reply blocked ordinary information")
	}
	complete(d, "sent")

	// Legacy/retry jobs are filtered by their specific permission, not just by
	// whether the actor still has some unrelated operator access.
	exec("INSERT INTO user_permissions(telegram_id,permission) VALUES(13,'table_viewer')")
	for _, tc := range []struct{ kind, body string }{
		{"view", `{"kind":"notice","code":"denied"}`},
		{"view", `{"kind":"notice","code":"unavailable"}`},
		{"view", `{"kind":"notice","code":"sync_denied"}`},
		{"view", `{"kind":"notice","code":"send_source"}`},
		{"view", `{"kind":"sync_done"}`},
		{"view", `{"kind":"broadcast_preview"}`},
		{"view", `{"kind":"broadcast_progress"}`},
		{"sync", `{"code":"staff"}`},
	} {
		var id int64
		must(db.QueryRow(ctx, "INSERT INTO outbound_messages(chat,actor,priority,kind,body,status,attempts) VALUES(13,13,'interactive',$1,$2,'retry_wait',2) RETURNING id", tc.kind, []byte(tc.body)).Scan(&id))
		if got := claim(id); got.Id != 0 || got.View != nil {
			t.Fatalf("queued admin reply exposed: %s", tc.body)
		}
		var state string
		var attempts int
		must(db.QueryRow(ctx, "SELECT status,attempts FROM outbound_messages WHERE id=$1", id).Scan(&state, &attempts))
		if state != "cancelled" || attempts != 2 || slices.Contains(pending(0), id) {
			t.Fatal("suppression lost terminal/retry state")
		}
	}

	// Two immediate replies retain their order, and both replay identically.
	u, r = send(1, "/poll all")
	if len(r.DeliveryIds) != 2 || r.DeliveryIds[0] >= r.DeliveryIds[1] {
		t.Fatal("preview reply order")
	}
	replayed, err := client.Accept(auth, u)
	must(err)
	if !slices.Equal(r.DeliveryIds, replayed.DeliveryIds) {
		t.Fatal("multi-reply replay changed")
	}
	exec("UPDATE outbound_messages SET status='sent'")

	// Continuations have no inbound association and still recover over gRPC.
	_, r = send(1, "/sync_staff_chat")
	d = claim(r.DeliveryIds[0])
	_, err = client.Complete(auth, &pb.Completion{Id: d.Id, Lease: d.Lease, Outcome: "sent", NextAfter: 10, Checked: 10})
	must(err)
	ids = pending(0)
	if len(ids) != 1 {
		t.Fatal("sync continuation lost")
	}
	d = claim(ids[0])
	if d.Kind != "sync" || d.View.Numbers[0] != 10 {
		t.Fatal("invalid continuation")
	}
	complete(d, "sent")
	ids = pending(0)
	if len(ids) != 1 {
		t.Fatal("sync result lost")
	}
	d = claim(ids[0])
	if d.View.Kind != "sync_done" {
		t.Fatal("invalid sync result")
	}
	complete(d, "sent")

	// Milestones are background jobs, not reply IDs, and recover without Kafka.
	exec("INSERT INTO bot_chats(chat_id,chat_type) VALUES(-200,'staff')")
	exec(`UPDATE users SET state='confirm',name='Fixture User',birth_date='10.03.2002',"group"='FIXTURE',phone='79991234567',expectations='Fixture',will_drive='Обязательно! 🤩',trip_attendance='Да, точно еду! ✅' WHERE telegram_id=10`)
	must(db.QueryRow(ctx, "SELECT version FROM users WHERE telegram_id=10").Scan(&version))
	confirmation := &pb.Update{Id: seq, Actor: 10, Chat: 10, ChatType: "private", Callback: "c:" + strconv.FormatInt(version, 10) + ":confirm", MessageId: 45, CallbackMessageEditable: true}
	r, err = client.Accept(auth, confirmation)
	seq++
	must(err)
	if len(r.DeliveryIds) != 1 {
		t.Fatal("confirmation must produce one edit, excluding background milestones")
	}
	ids = pending(0)
	if len(ids) != 2 {
		t.Fatal("milestone not recoverable")
	}
	for _, id := range ids {
		d := claim(id)
		if id == r.DeliveryIds[0] {
			if d.View.Kind != "registered" || d.View.Code != "registration_completed" || d.EditMessageId != 45 || len(d.View.Fields) != 0 {
				t.Fatal("confirmation must replace questionnaire with completion text")
			}
			labels := []string{}
			for _, button := range d.View.Buttons {
				labels = append(labels, button.LabelKey)
			}
			if !slices.Equal(labels, []string{"edit", "about_button", "bring_button"}) {
				t.Fatal("completion must retain registered-user buttons")
			}
		}
		if id != r.DeliveryIds[0] && (d.Chat != -200 || d.View.Kind != "milestone") {
			t.Fatal("incorrect background milestone")
		}
		complete(d, "sent")
	}
	replay, err = client.Accept(auth, confirmation)
	must(err)
	if !replay.Duplicate || !slices.Equal(replay.DeliveryIds, r.DeliveryIds) {
		t.Fatal("confirmation replay duplicated completion message")
	}
	_, r = send(10, "/start")
	for _, id := range r.DeliveryIds {
		d = claim(id)
		if d.View.Code == "registration_completed" {
			t.Fatal("reopening a registered form repeated completion message")
		}
		complete(d, "sent")
	}

	// Failure during durable reply creation rolls back the mutation and dedup.
	exec("ALTER TABLE outbound_messages ADD CONSTRAINT fixture_reject CHECK(chat<>99)")
	u = &pb.Update{Id: seq, Actor: 99, Chat: 99, ChatType: "private", Text: "/start"}
	_, err = client.Accept(auth, u)
	if status.Code(err) != codes.Unavailable {
		t.Fatal("failed reply accepted")
	}
	var count int
	must(db.QueryRow(ctx, "SELECT count(*) FROM processed_updates WHERE update_id=$1", seq).Scan(&count))
	if count != 0 {
		t.Fatal("update committed without reply")
	}
	exec("ALTER TABLE outbound_messages DROP CONSTRAINT fixture_reject")
	r, err = client.Accept(auth, u)
	must(err)
	if r.Duplicate || len(r.DeliveryIds) != 2 {
		t.Fatal("failed transaction not recoverable")
	}
	seq++

	// A valid Telegram Unicode message must reach domain validation, not become
	// a transport InvalidArgument that stalls polling/webhook acknowledgement.
	exec("INSERT INTO users(telegram_id,state,username) VALUES(200,'expectations','before')")
	_, unicodeReply := send(200, strings.Repeat("中", 3000)) // 9000 UTF-8 bytes.
	d = claim(unicodeReply.DeliveryIds[0])
	if d.View.Kind != "validation" || d.View.Code != "invalid_value" {
		t.Fatal("valid Telegram Unicode text rejected at ingress instead of domain validation")
	}
	complete(d, "sent")
	exec("UPDATE users SET username='before' WHERE telegram_id=200")

	for _, tc := range []struct {
		name   string
		change func(*pb.Update)
	}{
		{"oversized text", func(u *pb.Update) { u.Text = strings.Repeat("中", 4097) }},
		{"oversized ASCII", func(u *pb.Update) { u.Text = strings.Repeat("a", 9000) }},
		{"callback byte limit", func(u *pb.Update) { u.Callback = strings.Repeat("中", 22) }},
		{"oversized metadata", func(u *pb.Update) { u.DisplayName = strings.Repeat("a", 513) }},
		{"NUL metadata", func(u *pb.Update) { u.DisplayName = "invalid\x00name" }},
		{"NUL text", func(u *pb.Update) { u.Text = "invalid\x00text" }},
		{"NUL title", func(u *pb.Update) { u.ChatTitle = "invalid\x00title" }},
		{"unknown kind", func(u *pb.Update) { u.Kind = "unsupported" }},
	} {
		t.Log("permanent content regression:", tc.name)
		u := &pb.Update{Id: seq, Actor: 200, Chat: 200, ChatType: "private", Text: "/start poison", Username: "after"}
		seq++
		tc.change(u)
		r, err := client.Accept(auth, u)
		must(err)
		if r.Duplicate || len(r.DeliveryIds) != 1 {
			t.Fatal("poison not durably acknowledged")
		}
		if !slices.Contains(pending(0), r.DeliveryIds[0]) {
			t.Fatal("poison reply cannot recover")
		}
		replay, err := client.Accept(auth, u)
		must(err)
		if !replay.Duplicate || !slices.Equal(replay.DeliveryIds, r.DeliveryIds) {
			t.Fatal("poison replay not stable")
		}
		var username string
		must(db.QueryRow(ctx, "SELECT username,version FROM users WHERE telegram_id=200").Scan(&username, &version))
		if username != "before" || version != 0 {
			t.Fatal("poison mutated user")
		}
		must(db.QueryRow(ctx, "SELECT count(*) FROM first_starts WHERE telegram_id=200").Scan(&count))
		if count != 0 {
			t.Fatal("poison executed /start")
		}
		d := claim(r.DeliveryIds[0])
		if d.View.Kind != "notice" || d.View.Code != "invalid_content" || len(d.View.Fields) != 0 || d.EditMessageId != 0 {
			t.Fatal("poison reply not neutral")
		}
		complete(d, "sent")
		// Even a duplicate cannot bypass routing identity checks.
		u.Chat = 201
		_, err = client.Accept(auth, u)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatal("poison duplicate bypassed identity")
		}
	}
	// Progress after poison: no queue head or update-processing blockage remains.
	_, nextReply := send(200, "Valid answer")
	d = claim(nextReply.DeliveryIds[0])
	if d.Id == 0 {
		t.Fatal("poison blocked subsequent reply")
	}
	complete(d, "sent")
	var answer string
	must(db.QueryRow(ctx, "SELECT expectations FROM users WHERE telegram_id=200").Scan(&answer))
	if answer != "Valid answer" {
		t.Fatal("poison blocked subsequent update")
	}

	// Content rejection is not an acknowledgement if its durable notice fails.
	exec("ALTER TABLE outbound_messages ADD CONSTRAINT fixture_poison_reject CHECK(chat<>300)")
	u = &pb.Update{Id: seq, Actor: 300, Chat: 300, ChatType: "private", Text: strings.Repeat("a", 4097)}
	_, err = client.Accept(auth, u)
	if status.Code(err) != codes.Unavailable {
		t.Fatal("poison acknowledged without persistence")
	}
	must(db.QueryRow(ctx, "SELECT count(*) FROM processed_updates WHERE bot_id=1000 AND update_id=$1", seq).Scan(&count))
	if count != 0 {
		t.Fatal("poison dedup committed without notice")
	}
	exec("ALTER TABLE outbound_messages DROP CONSTRAINT fixture_poison_reject")
	r, err = client.Accept(auth, u)
	must(err)
	if r.Duplicate || len(r.DeliveryIds) != 1 {
		t.Fatal("poison rollback could not recover")
	}
}
