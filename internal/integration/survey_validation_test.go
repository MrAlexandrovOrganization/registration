//go:build integration

package integration

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

func TestSurveyContactValidation(t *testing.T) {
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	must(err)
	if cfg.ConnConfig.Database != "registration_test" {
		t.Fatal("requires disposable registration_test")
	}
	bootstrap, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer bootstrap.Close()
	_, err = bootstrap.Exec(ctx, "CREATE SCHEMA survey_validation_test")
	must(err)
	cfg.ConnConfig.RuntimeParams["search_path"] = "survey_validation_test"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer db.Close()
	must(store.Migrate(ctx, db))
	_, err = db.Exec(ctx, `INSERT INTO users(telegram_id,state,name,birth_date,"group",phone,expectations) VALUES(42,'new','Fixture','01.01.2000','TEST','9991234567','Fixture')`)
	must(err)
	svc := &service.Service{DB: db, BotID: 1000}
	steps := []struct {
		text, phone, kind, field, code, savedGroup, savedPhone string
	}{
		{"/start", "", "question", "group", "", "TEST", "9991234567"},
		{"M9-11", "", "validation", "group", "invalid_group", "TEST", "9991234567"},
		{" иу7-41 ", "", "question", "phone", "", "ИУ7-41", "9991234567"},
		{"+12025550123", "", "validation", "phone", "invalid_phone", "ИУ7-41", "9991234567"},
		{"", "9991234567", "validation", "phone", "invalid_phone", "ИУ7-41", "9991234567"},
		{"", "+79991234567", "confirm", "confirm", "", "ИУ7-41", "79991234567"},
	}
	for i, step := range steps {
		r, err := svc.Accept(ctx, &pb.Update{Id: int64(i + 1), Actor: 42, Chat: 42, ChatType: "private", Kind: "message", Text: step.text, Phone: step.phone, ContactOwner: 42})
		must(err)
		if len(r.DeliveryIds) == 0 {
			t.Fatal("missing durable response")
		}
		var body []byte
		must(db.QueryRow(ctx, "SELECT body FROM outbound_messages WHERE id=$1", r.DeliveryIds[len(r.DeliveryIds)-1]).Scan(&body))
		v := &pb.View{}
		must(protojson.Unmarshal(body, v))
		if v.Kind != step.kind || v.Field != step.field || v.Code != step.code {
			t.Fatalf("step %d: view=%v", i, v)
		}
		var group, phone string
		must(db.QueryRow(ctx, `SELECT "group",phone FROM users WHERE telegram_id=42`).Scan(&group, &phone))
		if group != step.savedGroup || phone != step.savedPhone {
			t.Fatalf("step %d: unexpected saved answers", i)
		}
	}
}
