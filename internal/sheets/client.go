// Package sheets maintains a dedicated Google worksheet as an export mirror.
package sheets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"registration.local/backend/internal/service"
)

const scope = "https://www.googleapis.com/auth/spreadsheets"

type Client struct {
	http          *http.Client
	endpoint      string
	SpreadsheetID string
	SheetID       int64
}

func New(ctx context.Context, credentialsFile, spreadsheetID string, sheetID int64) (*Client, error) {
	f, err := os.Open(credentialsFile)
	if err != nil {
		return nil, errors.New("cannot open Google Sheets credentials")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, errors.New("cannot read Google Sheets credentials")
	}
	cfg, err := google.JWTConfigFromJSON(data, scope)
	if err != nil || cfg.Email == "" || len(cfg.PrivateKey) == 0 || cfg.Subject != "" {
		return nil, errors.New("Google Sheets requires service account credentials")
	}
	// Bound token exchange as well as Sheets calls; never include OAuth errors in logs.
	authCtx := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 10 * time.Second})
	client := cfg.Client(authCtx)
	client.Timeout = 30 * time.Second
	return &Client{http: client, endpoint: "https://sheets.googleapis.com/v4/spreadsheets/", SpreadsheetID: spreadsheetID, SheetID: sheetID}, nil
}

type cell struct {
	Value struct {
		Text string `json:"stringValue"`
	} `json:"userEnteredValue"`
}
type row struct {
	Values []cell `json:"values"`
}

// Replace uses one atomic batch: clear all previous values, grow the grid if
// needed, write literal strings, and format the new range. Other tabs are untouched.
func (c *Client) Replace(ctx context.Context, table service.ExportTable) error {
	if len(table.Rows) == 0 || len(table.Rows[0]) == 0 || len(table.Widths) != len(table.Rows[0]) {
		return errors.New("invalid Sheets export")
	}
	columns := len(table.Rows[0])
	rows := make([]row, len(table.Rows))
	for n, values := range table.Rows {
		if len(values) != columns {
			return errors.New("invalid Sheets row")
		}
		rows[n].Values = make([]cell, columns)
		for i, value := range values {
			rows[n].Values[i].Value.Text = value
		}
	}
	// Read only grid dimensions. Never fetch or log participant values from Google.
	var metadata struct {
		Sheets []struct {
			Properties struct {
				ID   int64 `json:"sheetId"`
				Grid struct {
					Rows    int `json:"rowCount"`
					Columns int `json:"columnCount"`
				} `json:"gridProperties"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	query := "?fields=sheets.properties(sheetId,gridProperties(rowCount,columnCount))"
	if err := c.request(ctx, http.MethodGet, query, nil, &metadata); err != nil {
		return err
	}
	gridRows, gridColumns := 0, 0
	found := false
	for _, sheet := range metadata.Sheets {
		if sheet.Properties.ID == c.SheetID {
			gridRows = sheet.Properties.Grid.Rows
			gridColumns = sheet.Properties.Grid.Columns
			found = true
		}
	}
	if !found {
		return errors.New("configured Google worksheet not found")
	}
	gridRows = max(gridRows, len(rows))
	gridColumns = max(gridColumns, columns)
	full := map[string]any{"sheetId": c.SheetID}
	used := map[string]any{"sheetId": c.SheetID, "endRowIndex": len(rows), "endColumnIndex": columns}
	header := map[string]any{"sheetId": c.SheetID, "endRowIndex": 1, "endColumnIndex": columns}
	requests := []any{
		map[string]any{"updateSheetProperties": map[string]any{"properties": map[string]any{"sheetId": c.SheetID, "gridProperties": map[string]any{"rowCount": gridRows, "columnCount": gridColumns, "frozenRowCount": 1}}, "fields": "gridProperties.rowCount,gridProperties.columnCount,gridProperties.frozenRowCount"}},
		map[string]any{"updateCells": map[string]any{"range": full, "fields": "userEnteredValue"}},
		map[string]any{"updateCells": map[string]any{"range": used, "rows": rows, "fields": "userEnteredValue"}},
		map[string]any{"repeatCell": map[string]any{"range": used, "cell": map[string]any{"userEnteredFormat": map[string]any{"numberFormat": map[string]any{"type": "TEXT"}, "wrapStrategy": "WRAP", "verticalAlignment": "TOP"}}, "fields": "userEnteredFormat.numberFormat,userEnteredFormat.wrapStrategy,userEnteredFormat.verticalAlignment"}},
		map[string]any{"repeatCell": map[string]any{"range": header, "cell": map[string]any{"userEnteredFormat": map[string]any{"textFormat": map[string]any{"bold": true}, "backgroundColor": map[string]any{"red": 0.85, "green": 0.91, "blue": 0.98}}}, "fields": "userEnteredFormat.textFormat,userEnteredFormat.backgroundColor"}},
		map[string]any{"setBasicFilter": map[string]any{"filter": map[string]any{"range": used}}},
	}
	for i, width := range table.Widths {
		requests = append(requests, map[string]any{"updateDimensionProperties": map[string]any{"range": map[string]any{"sheetId": c.SheetID, "dimension": "COLUMNS", "startIndex": i, "endIndex": i + 1}, "properties": map[string]any{"pixelSize": int(width * 7)}, "fields": "pixelSize"}})
	}
	body, err := json.Marshal(map[string]any{"requests": requests})
	if err != nil {
		return errors.New("cannot encode Sheets export")
	}
	if len(body) > 16<<20 {
		return errors.New("Sheets export exceeds 16 MiB")
	}
	return c.request(ctx, http.MethodPost, ":batchUpdate", body, nil)
}

func (c *Client) request(ctx context.Context, method, suffix string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+url.PathEscape(c.SpreadsheetID)+suffix, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create Sheets request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("Google Sheets request failed")
	}
	defer resp.Body.Close()
	// API errors can contain personal data and OAuth responses can contain tokens.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("Google Sheets returned unsuccessful status")
	}
	if out != nil {
		if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return errors.New("invalid Sheets metadata")
		}
	}
	return nil
}
