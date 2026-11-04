package service

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/xuri/excelize/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/resources"
	"registration.local/backend/internal/store"
)

// ExportTable is the common, literal-text representation for XLSX and Sheets.
// Rows include the header. Callers own the repeatable-read transaction.
type ExportTable struct {
	Rows        [][]string
	Widths      []float64
	SurveyStart int
}

func (s *Service) ExportTable(ctx context.Context, tx pgx.Tx) (ExportTable, error) {
	table := ExportTable{}
	columns, answers, err := loadSurveyExport(ctx, tx)
	if err != nil {
		return table, err
	}
	finalRows, err := tx.Query(ctx, store.Q("final_export"))
	if err != nil {
		return table, err
	}
	finalAnswers := map[int64]string{}
	for finalRows.Next() {
		var actor int64
		var state, positiveLabel, negativeLabel string
		if err = finalRows.Scan(&actor, &state, &positiveLabel, &negativeLabel); err != nil {
			finalRows.Close()
			return table, err
		}
		finalAnswers[actor] = resources.FinalConfirmationAnswer(state, positiveLabel, negativeLabel)
	}
	err = finalRows.Err()
	finalRows.Close()
	if err != nil {
		return table, err
	}
	headers := []string{"telegram_id", "username", "telegram_sername", "name", "birth_date", "group", "phone", "expectations", "will_drive", "trip_attendance", "is_staff", "is_counselor", "is_blocked", "first_start_source", "has_registered", "first_start_at_utc"}
	header := []string{}
	for i, h := range headers {
		if !s.ParticipationEnabled && (i == 8 || i == 9) {
			continue
		}
		header = append(header, h)
		width := 22.0
		switch h {
		case "name", "telegram_sername":
			width = 36
		case "expectations":
			width = 60
		case "first_start_at_utc":
			width = 24
		}
		table.Widths = append(table.Widths, width)
	}
	header = append(header, resources.FinalConfirmationHeader)
	table.Widths = append(table.Widths, 36)
	table.SurveyStart = len(header)
	for _, q := range columns {
		header = append(header, resources.SurveyAnswerHeader(q.title, q.id))
		table.Widths = append(table.Widths, 42)
	}
	table.Rows = append(table.Rows, header)
	rows, err := tx.Query(ctx, store.Q("export"))
	if err != nil {
		return table, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(table.Rows) > 50000 {
			return table, status.Error(codes.ResourceExhausted, "export row limit")
		}
		values, err := rows.Values()
		if err != nil {
			return table, err
		}
		row := make([]string, 0, len(header))
		for i, v := range values {
			if !s.ParticipationEnabled && (i == 8 || i == 9) {
				continue
			}
			text := ""
			switch value := v.(type) {
			case string:
				text = value
			case int64:
				text = strconv.FormatInt(value, 10)
			case int32:
				text = strconv.FormatInt(int64(value), 10)
			}
			row = append(row, text)
		}
		actor := values[0].(int64)
		row = append(row, finalAnswers[actor])
		for _, q := range columns {
			row = append(row, answers[actor][q.id])
		}
		table.Rows = append(table.Rows, row)
	}
	return table, rows.Err()
}

func (s *Service) Export(ctx context.Context, r *pb.ExportRequest) (*pb.ExportResponse, error) {
	fail := func(err error) (*pb.ExportResponse, error) { return nil, persistenceError(ctx, err) }
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback(ctx)
	if !s.allowed(ctx, tx, r.Actor, "table_viewer") {
		return nil, status.Error(codes.PermissionDenied, "permission denied")
	}
	table, err := s.ExportTable(ctx, tx)
	if err != nil {
		return fail(err)
	}
	file := excelize.NewFile()
	defer file.Close()
	style, err := file.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	if err != nil {
		return fail(err)
	}
	for i, width := range table.Widths {
		col, _ := excelize.ColumnNumberToName(i + 1)
		if err = file.SetColWidth("Sheet1", col, col, width); err != nil {
			return fail(err)
		}
	}
	for n, row := range table.Rows {
		for i, text := range row {
			cell, _ := excelize.CoordinatesToCellName(i+1, n+1)
			// Literal strings prevent formulas and preserve phone numbers/IDs/dates.
			if err = file.SetCellStr("Sheet1", cell, text); err != nil {
				return fail(err)
			}
			if n == 0 && i >= table.SurveyStart {
				if err = file.SetCellStyle("Sheet1", cell, cell, style); err != nil {
					return fail(err)
				}
			}
		}
	}
	if len(table.Rows[0]) > table.SurveyStart {
		if err = file.SetRowHeight("Sheet1", 1, 60); err != nil {
			return fail(err)
		}
	}
	if _, err = tx.Exec(ctx, store.Q("audit"), r.Actor, "export", 0); err != nil {
		return fail(err)
	}
	buffer, err := file.WriteToBuffer()
	if err != nil {
		return fail(err)
	}
	if buffer.Len() > 12<<20 {
		return nil, status.Error(codes.ResourceExhausted, "export byte limit")
	}
	if err = tx.Commit(ctx); err != nil {
		return fail(err)
	}
	return &pb.ExportResponse{Xlsx: buffer.Bytes()}, nil
}
