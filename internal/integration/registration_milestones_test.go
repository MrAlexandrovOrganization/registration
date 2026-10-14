//go:build integration

package integration

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

func TestRegistrationMilestones(t *testing.T) {
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
	_, err = bootstrap.Exec(ctx, "CREATE SCHEMA registration_milestones_test")
	must(err)
	cfg.ConnConfig.RuntimeParams["search_path"] = "registration_milestones_test"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer db.Close()
	must(store.Migrate(ctx, db))

	_, err = db.Exec(ctx, `INSERT INTO users(telegram_id,state,version)
		SELECT 10000 + id, 'registered', 0 FROM generate_series(1,162) AS ids(id);
		INSERT INTO users(telegram_id,state,is_blocked,is_staff,is_counselor,version) VALUES
			(20001,'registered',1,0,0,20),
			(20002,'registered',0,1,0,20),
			(20003,'registered',0,0,1,20),
			(10163,'name',0,0,0,20),
			(20005,'name',0,0,0,20);`)
	must(err)
	_, err = db.Exec(ctx, `INSERT INTO users(telegram_id,state,name,birth_date,"group",phone,expectations,version)
		VALUES(20004,'confirm','Fixture User','01.01.2000','ИУ7-41','79991234567','Fixture',20)`)
	must(err)
	_, err = db.Exec(ctx, "INSERT INTO users(telegram_id,state,version) VALUES(10164,'registered',20)")
	must(err)
	_, err = db.Exec(ctx, "INSERT INTO users(telegram_id,state,version) VALUES(1,'new',0)")
	must(err)
	_, err = db.Exec(ctx, `INSERT INTO bot_chats(chat_id,chat_type,chat_title) VALUES
			(-400,'staff','Existing staff chat'),(-600,'counselor','Existing counselor chat');
		INSERT INTO outbound_messages(chat,actor,priority,kind,body,status)
			VALUES(10163,10163,'interactive','view','{"kind":"confirm"}','sent');
		INSERT INTO outbound_messages(chat,priority,kind,body) VALUES(-400,'interactive','view','{"kind":"milestone","numbers":[25]}');`)
	must(err)
	_, err = db.Exec(ctx, "DROP TABLE registration_milestone_notifications; ALTER TABLE users DROP COLUMN has_registered, DROP COLUMN staff_membership_checked, DROP COLUMN counselor_membership_checked; ALTER TABLE bot_chats DROP COLUMN milestone_ready; DELETE FROM schema_migrations WHERE version>=5")
	must(err)
	must(store.Migrate(ctx, db))
	var oldMilestoneStatus string
	must(db.QueryRow(ctx, "SELECT status FROM outbound_messages WHERE chat=-400").Scan(&oldMilestoneStatus))
	if oldMilestoneStatus != "cancelled" {
		t.Fatal("migration must retire queued participation-based milestone views")
	}
	var hasRegistered bool
	must(db.QueryRow(ctx, "SELECT has_registered FROM users WHERE telegram_id=10163").Scan(&hasRegistered))
	if !hasRegistered {
		t.Fatal("migration must recognize a completed questionnaire while the user is editing")
	}
	must(db.QueryRow(ctx, "SELECT has_registered FROM users WHERE telegram_id=20004").Scan(&hasRegistered))
	if !hasRegistered {
		t.Fatal("a complete questionnaire at the final confirmation step should count")
	}
	must(db.QueryRow(ctx, "SELECT has_registered FROM users WHERE telegram_id=20005").Scan(&hasRegistered))
	if hasRegistered {
		t.Fatal("a high version on an incomplete form must not count as registration")
	}
	svc := &service.Service{DB: db, RootID: 1, BotID: 1000}
	seq := int64(1)
	send := func(actor, chat int64, chatType, text, callback string) *pb.Receipt {
		t.Helper()
		receipt, err := svc.Accept(ctx, &pb.Update{
			Id: seq, Actor: actor, Chat: chat, ChatType: chatType, Text: text, Callback: callback,
			ChatTitle: "Registration staff",
		})
		seq++
		must(err)
		return receipt
	}
	viewFor := func(id int64) *pb.View {
		t.Helper()
		var body []byte
		must(db.QueryRow(ctx, "SELECT body FROM outbound_messages WHERE id=$1", id).Scan(&body))
		view := &pb.View{}
		must(protojson.Unmarshal(body, view))
		return view
	}

	registered := send(1, -500, "supergroup", "/register_staff_chat", "")
	if len(registered.DeliveryIds) != 1 {
		t.Fatalf("staff chat registration should confirm setup and request membership sync, got %d replies", len(registered.DeliveryIds))
	}
	if view := viewFor(registered.DeliveryIds[0]); view.Kind != "notice" || view.Code != "staff_sync_required" {
		t.Fatalf("unexpected setup confirmation: %v", view)
	}
	var initialMessages int
	must(db.QueryRow(ctx, "SELECT count(*) FROM outbound_messages WHERE chat=-500 AND body->>'kind'='registration_count'").Scan(&initialMessages))
	if initialMessages != 0 {
		t.Fatal("initial count must wait until staff membership flags are synchronized")
	}
	var milestoneReady bool
	must(db.QueryRow(ctx, "SELECT milestone_ready FROM bot_chats WHERE chat_id=-500").Scan(&milestoneReady))
	if milestoneReady {
		t.Fatal("initial count and milestone notifications must wait for staff membership sync")
	}

	memberUpdate := &pb.Update{Id: seq, Actor: 1, Chat: -500, ChatType: "supergroup", Kind: "member", MemberId: 10164, MemberActive: true}
	seq++
	_, err = svc.Accept(ctx, memberUpdate)
	must(err)
	syncReceipt := send(1, 1, "private", "/sync_staff_chat", "")
	if len(syncReceipt.DeliveryIds) != 1 {
		t.Fatal("staff sync should enqueue one durable job")
	}
	_, err = db.Exec(ctx, "UPDATE runtime_state SET sender_until=now()-interval '1 second'")
	must(err)
	syncDelivery, err := svc.Claim(ctx, &pb.ClaimRequest{Id: syncReceipt.DeliveryIds[0], Worker: "registration-test-worker"})
	must(err)
	if syncDelivery.Kind != "sync" || syncDelivery.View.Field != "registration_count" {
		t.Fatal("pending setup sync must finalize the initial registration count")
	}
	_, err = svc.Complete(ctx, &pb.Completion{Id: syncDelivery.Id, Lease: syncDelivery.Lease, Outcome: "sent", Checked: 1})
	must(err)
	var initialBody []byte
	must(db.QueryRow(ctx, `SELECT body FROM outbound_messages WHERE chat=-500 AND body->>'kind'='registration_count'`).Scan(&initialBody))
	initialView := &pb.View{}
	must(protojson.Unmarshal(initialBody, initialView))
	if len(initialView.Numbers) != 1 || initialView.Numbers[0] != 164 {
		t.Fatalf("incorrect post-sync registration count: %v", initialView)
	}
	must(db.QueryRow(ctx, "SELECT milestone_ready FROM bot_chats WHERE chat_id=-500").Scan(&milestoneReady))
	if !milestoneReady {
		t.Fatal("successful setup sync must enable milestone notifications")
	}
	var baselineCount int
	must(db.QueryRow(ctx, "SELECT count(*) FROM registration_milestone_notifications").Scan(&baselineCount))
	if baselineCount != 16 {
		t.Fatalf("initial count 164 should baseline thresholds 10..160, got %d", baselineCount)
	}

	_, err = db.Exec(ctx, `INSERT INTO users(telegram_id,state,name,birth_date,"group",phone,version)
		VALUES(30000,'expectations','Fixture User','01.01.2000','ИУ7-41','79991234567',1)`)
	must(err)
	completedForm := send(30000, 30000, "private", "Fixture expectations", "")
	if len(completedForm.DeliveryIds) != 4 {
		t.Fatal("a new user should have staff and counselor membership checks before the form replies")
	}
	for _, jobID := range completedForm.DeliveryIds[:2] {
		membershipDelivery, err := svc.Claim(ctx, &pb.ClaimRequest{Id: jobID, Worker: "registration-test-worker"})
		must(err)
		groupChats := map[string]int64{"staff": -500, "counselor": -600}
		if membershipDelivery.Kind != "membership_check" || membershipDelivery.SourceChat != groupChats[membershipDelivery.View.Code] || membershipDelivery.SourceMessage != 30000 {
			t.Fatalf("unexpected new-user membership check: %v", membershipDelivery)
		}
		_, err = svc.SyncMember(ctx, &pb.SyncMemberRequest{Actor: 1, Chat: membershipDelivery.SourceChat, User: 30000, Active: false})
		must(err)
		_, err = svc.Complete(ctx, &pb.Completion{Id: membershipDelivery.Id, Lease: membershipDelivery.Lease, Outcome: "sent"})
		must(err)
	}
	var notifications int
	must(db.QueryRow(ctx, `SELECT count(*) FROM outbound_messages WHERE kind='view' AND body->>'kind'='registration_milestone'`).Scan(&notifications))
	if notifications != 0 {
		t.Fatal("no threshold should be emitted at 165 registrations")
	}
	var count int64
	var state string
	must(db.QueryRow(ctx, "SELECT has_registered FROM users WHERE telegram_id=30000").Scan(&hasRegistered))
	if !hasRegistered {
		t.Fatal("a fully completed questionnaire must count before the final confirm button")
	}
	must(db.QueryRow(ctx, "SELECT state FROM users WHERE telegram_id=30000").Scan(&state))
	if state != "confirm" {
		t.Fatalf("expected final confirmation step, got state %q", state)
	}
	var version int64
	must(db.QueryRow(ctx, "SELECT version FROM users WHERE telegram_id=30000").Scan(&version))
	send(30000, 30000, "private", "", "c:"+strconv.FormatInt(version, 10)+":edit")
	must(db.QueryRow(ctx, "SELECT version FROM users WHERE telegram_id=30000").Scan(&version))
	send(30000, 30000, "private", "", "c:"+strconv.FormatInt(version, 10)+":edit_name")
	must(db.QueryRow(ctx, "SELECT state FROM users WHERE telegram_id=30000").Scan(&state))
	if state != "edit_name" {
		t.Fatalf("expected active name edit, got state %q", state)
	}
	must(db.QueryRow(ctx, store.Q("registration_count")).Scan(&count))
	if count != 165 {
		t.Fatalf("editing a previously registered participant must not lower the count, got %d", count)
	}
	send(30000, 30000, "private", "Updated Fixture User", "")
	must(db.QueryRow(ctx, store.Q("registration_count")).Scan(&count))
	if count != 165 {
		t.Fatalf("saving an edit before reconfirmation must preserve the completed-registration count, got %d", count)
	}
	must(db.QueryRow(ctx, "SELECT version FROM users WHERE telegram_id=30000").Scan(&version))
	send(30000, 30000, "private", "", "c:"+strconv.FormatInt(version, 10)+":confirm")
	must(db.QueryRow(ctx, store.Q("registration_count")).Scan(&count))
	if count != 165 {
		t.Fatalf("reconfirming an existing participant must not add another registration, got %d", count)
	}

	_, err = db.Exec(ctx, `INSERT INTO users(telegram_id,state,has_registered,staff_membership_checked,counselor_membership_checked)
		SELECT 40000 + id, 'registered',true,true,true FROM generate_series(1,4) AS ids(id)`)
	must(err)
	send(30000, 30000, "private", "/start", "") // 169: still below the next threshold.
	_, err = db.Exec(ctx, "INSERT INTO users(telegram_id,state,has_registered,staff_membership_checked,counselor_membership_checked) VALUES(50000,'registered',true,true,true)")
	must(err)
	send(30000, 30000, "private", "/start", "") // 170: first notification after the 164 snapshot.
	must(db.QueryRow(ctx, `SELECT count(*) FROM outbound_messages WHERE chat=-500 AND kind='view'
		AND body->>'kind'='registration_milestone' AND body->'numbers'->>0='170'`).Scan(&notifications))
	if notifications != 1 {
		t.Fatalf("expected one 170 milestone, got %d", notifications)
	}
	send(30000, 30000, "private", "/start", "")
	must(db.QueryRow(ctx, `SELECT count(*) FROM outbound_messages WHERE body->>'kind'='registration_milestone'`).Scan(&notifications))
	if notifications != 1 {
		t.Fatal("reprocessing the same count must not repeat a milestone")
	}

	_, err = db.Exec(ctx, `INSERT INTO users(telegram_id,state,has_registered,staff_membership_checked,counselor_membership_checked)
		SELECT 60000 + id, 'registered',true,true,true FROM generate_series(1,10) AS ids(id)`)
	must(err)
	send(30000, 30000, "private", "/start", "") // 180: catch up exactly the newly reached threshold.
	must(db.QueryRow(ctx, `SELECT count(*) FROM outbound_messages WHERE chat=-500 AND kind='view'
		AND body->>'kind'='registration_milestone' AND body->'numbers'->>0='180'`).Scan(&notifications))
	if notifications != 1 {
		t.Fatalf("expected one 180 milestone, got %d", notifications)
	}
	must(db.QueryRow(ctx, `SELECT count(*) FROM outbound_messages WHERE kind='view' AND body->>'kind'='registration_milestone'`).Scan(&notifications))
	if notifications != 2 {
		t.Fatalf("only 170 and 180 should be reported after the initial 164 count, got %d", notifications)
	}

	memberUpdate = &pb.Update{Id: seq, Actor: 1, Chat: -500, ChatType: "supergroup", Kind: "member", MemberId: 10001, MemberActive: true}
	seq++
	_, err = svc.Accept(ctx, memberUpdate)
	must(err)
	var isStaff int
	must(db.QueryRow(ctx, "SELECT is_staff FROM users WHERE telegram_id=10001").Scan(&isStaff))
	if isStaff != 1 {
		t.Fatal("membership updates in the registered staff chat must mark known users as staff")
	}
	must(db.QueryRow(ctx, store.Q("registration_count")).Scan(&count))
	if count != 199 {
		t.Fatalf("staff membership must exclude the person from registration count, got %d", count)
	}
}
