//go:build integration

package integration

import (
	"testing"

	"registration.local/backend/internal/store"
)

func TestFinalButtonLabelsUpgradeFromV8(t *testing.T) {
	f := newSurveyFixture(t, "final_labels_upgrade_test")
	q, positive := seedFinalConfirmation(f, 3)
	// Reconstruct the deployed v8 objects. Existing queued messages retain the
	// old resource-key buttons, including their delivery and answer statuses.
	f.exec(`ALTER TABLE final_confirmation_campaign DROP CONSTRAINT final_campaign_labels,
DROP COLUMN positive_label,DROP COLUMN negative_label;
ALTER TABLE final_confirmation_drafts DROP CONSTRAINT final_draft_labels,
DROP COLUMN positive_label,DROP COLUMN negative_label;
DELETE FROM schema_migrations WHERE version=9`)
	f.seeded(2, "new")
	f.seeded(3, "new")
	var draft, job int64
	f.must(f.db.QueryRow(f.ctx, `INSERT INTO final_confirmation_drafts
(actor,question_id,positive_option_id,source_chat,source_message,preview_chat,preview_message)
VALUES(1,$1,$2,1,350,1,777) RETURNING id`, q, positive).Scan(&draft))
	f.exec(`INSERT INTO final_confirmation_drafts(actor,question_id,positive_option_id,source_chat,source_message)
VALUES(2,$1,$2,2,351),(3,$1,$2,0,0)`, q, positive)
	f.exec(`INSERT INTO final_confirmation_campaign(id,question_id,positive_option_id,actor,draft_id,source_chat,source_message)
VALUES(1,$1,$2,1,$3,1,777)`, q, positive, draft)
	f.must(f.db.QueryRow(f.ctx, `INSERT INTO outbound_messages(chat,actor,priority,kind,source_chat,source_message,body)
VALUES(9998,9998,'interactive','copy',1,777,
'{"kind":"notice","code":"final_invitation","buttons":[{"labelKey":"final_yes","data":"final:yes"},{"labelKey":"final_no","data":"final:no"}]}') RETURNING id`).Scan(&job))
	f.exec("INSERT INTO final_confirmation_invitations(telegram_id,status,delivery_id) VALUES(9999,'yes',NULL),(9998,'invited',$1)", job)
	const snapshot = `SELECT jsonb_build_object(
'users',(SELECT jsonb_agg(to_jsonb(u) ORDER BY telegram_id) FROM users u),
'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM outbound_messages o),
'invitations',(SELECT jsonb_agg(to_jsonb(i) ORDER BY telegram_id) FROM final_confirmation_invitations i),
'campaign',(SELECT jsonb_agg(to_jsonb(c)-'positive_label'-'negative_label') FROM final_confirmation_campaign c),
'drafts',(SELECT jsonb_agg(to_jsonb(d)-'positive_label'-'negative_label' ORDER BY actor) FROM final_confirmation_drafts d),
'history',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<=8)
)::text`
	var before, after string
	f.must(f.db.QueryRow(f.ctx, snapshot).Scan(&before))
	if store.CheckSchema(f.ctx, f.db) == nil {
		t.Fatal("v8 must require the label migration")
	}
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.Migrate(f.ctx, f.db))
	f.must(store.CheckSchema(f.ctx, f.db))
	f.must(f.db.QueryRow(f.ctx, snapshot).Scan(&after))
	if before != after {
		t.Fatal("label migration changed existing data, delivery state or history")
	}
	var yes, no string
	f.must(f.db.QueryRow(f.ctx, "SELECT positive_label,negative_label FROM final_confirmation_campaign").Scan(&yes, &no))
	if yes != "Да" || no != "Нет" {
		t.Fatal("existing campaign labels changed")
	}
	f.must(f.db.QueryRow(f.ctx, "SELECT positive_label,negative_label FROM final_confirmation_drafts WHERE actor=1").Scan(&yes, &no))
	if yes != "Да" || no != "Нет" {
		t.Fatal("already reviewed preview lost its original labels")
	}
	var missing int
	f.must(f.db.QueryRow(f.ctx, "SELECT count(*) FROM final_confirmation_drafts WHERE actor IN (2,3) AND positive_label='' AND negative_label=''").Scan(&missing))
	if missing != 2 || finalCount(f, "true") != 2 {
		t.Fatal("unfinished drafts skipped label input or migration invited participants")
	}
	delivery := f.claim(job)
	if delivery.View.Buttons[0].LabelKey != "final_yes" || delivery.View.Buttons[0].Data != "final:yes" {
		t.Fatal("legacy invitation cannot be delivered")
	}
}
