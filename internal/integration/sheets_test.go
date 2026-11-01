//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/xuri/excelize/v2"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/sheets"
	"registration.local/backend/internal/store"
)

type sheetRecorder struct {
	table service.ExportTable
	fail  bool
	calls int
	block func()
}

func (r *sheetRecorder) Replace(_ context.Context, table service.ExportTable) error {
	r.calls++
	if r.block != nil {
		r.block()
	}
	if r.fail {
		return errors.New("synthetic Google outage")
	}
	r.table = table
	return nil
}

func TestSheetsMirrorRecoveryAndExportParity(t *testing.T) {
	f := newSurveyFixture(t, "sheets_sync_test")
	f.send(9, "/start fixture", "")
	recorder := &sheetRecorder{}
	worker := &sheets.Worker{Service: f.svc, Writer: recorder}
	check := func() {
		t.Helper()
		written, err := worker.Sync(f.ctx)
		f.must(err)
		if !written {
			t.Fatal("snapshot skipped")
		}
		exported, err := f.svc.Export(f.ctx, &pb.ExportRequest{Actor: 1})
		f.must(err)
		file, err := excelize.OpenReader(bytes.NewReader(exported.Xlsx))
		f.must(err)
		defer file.Close()
		rows, err := file.GetRows("Sheet1")
		f.must(err)
		// Excel omits trailing empty cells when reading rows.
		for i := range rows {
			for len(rows[i]) < len(recorder.table.Rows[i]) {
				rows[i] = append(rows[i], "")
			}
		}
		if !reflect.DeepEqual(rows, recorder.table.Rows) {
			t.Fatal("Sheets and XLSX representations differ")
		}
	}
	check()
	recorder.fail = true
	f.exec("UPDATE users SET name='Changed', phone='07001234567' WHERE telegram_id=9")
	if _, err := worker.Sync(f.ctx); err == nil {
		t.Fatal("outage must be reported")
	}
	recorder.fail = false
	check()
	if recorder.table.Rows[1][3] != "Changed" {
		t.Fatal("change lost after failed write")
	}
	// A separate worker/process still sees state after restart; no volatile cursor.
	worker = &sheets.Worker{Service: f.svc, Writer: recorder}
	f.exec("DELETE FROM first_starts WHERE telegram_id=9")
	f.exec("DELETE FROM users WHERE telegram_id=9")
	check()
	if len(recorder.table.Rows) != 1 {
		t.Fatal("deleted participant retained")
	}
}

func TestSheetsWritersSerialize(t *testing.T) {
	f := newSurveyFixture(t, "sheets_lock_test")
	tx, err := f.db.BeginTx(f.ctx, pgx.TxOptions{})
	f.must(err)
	defer tx.Rollback(f.ctx)
	var locked bool
	f.must(tx.QueryRow(f.ctx, store.Q("sheets_sync_lock")).Scan(&locked))
	if !locked {
		t.Fatal("fixture lock unavailable")
	}
	recorder := &sheetRecorder{}
	worker := &sheets.Worker{Service: f.svc, Writer: recorder}
	written, err := worker.Sync(f.ctx)
	f.must(err)
	if written || recorder.calls != 0 {
		t.Fatal("concurrent writer bypassed lock")
	}
	f.must(tx.Rollback(f.ctx))
	recorder.block = func() {
		other, err := f.db.Begin(f.ctx)
		f.must(err)
		defer other.Rollback(f.ctx)
		var acquired bool
		f.must(other.QueryRow(f.ctx, store.Q("sheets_sync_lock")).Scan(&acquired))
		if acquired {
			t.Fatal("writer released lock before Google write completed")
		}
	}
	written, err = worker.Sync(f.ctx)
	f.must(err)
	if !written {
		t.Fatal("lock not recovered")
	}
}
