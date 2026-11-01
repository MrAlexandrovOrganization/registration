package app

import (
	"strings"
	"testing"
)

func TestSheetsConfiguration(t *testing.T) {
	for k, v := range map[string]string{"DATABASE_URL": "postgres://fixture:fixture@localhost/registration_test", "BACKEND_TOKEN": strings.Repeat("x", 32), "ROOT_ID": "1001", "BOT_ID": "1000", "ALLOW_INSECURE_GRPC": "true", "GRPC_TLS_CERT": "", "GRPC_TLS_KEY": "", "BROADCAST_TRANSPORT": "postgres", "GOOGLE_SHEETS_SPREADSHEET_ID": "", "GOOGLE_SHEETS_CREDENTIALS_FILE": "", "GOOGLE_SHEETS_SHEET_ID": "0", "GOOGLE_SHEETS_SYNC_INTERVAL": "1m"} {
		t.Setenv(k, v)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_SHEETS_SPREADSHEET_ID", "fixture-book")
	if _, err := Load(); err == nil {
		t.Fatal("partial configuration accepted")
	}
	t.Setenv("GOOGLE_SHEETS_CREDENTIALS_FILE", "/synthetic/key.json")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"bad", "1s", "25h"} {
		t.Setenv("GOOGLE_SHEETS_SYNC_INTERVAL", v)
		if _, err := Load(); err == nil {
			t.Fatal("invalid interval accepted")
		}
	}
	t.Setenv("GOOGLE_SHEETS_SYNC_INTERVAL", "1m")
	t.Setenv("GOOGLE_SHEETS_SHEET_ID", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("negative worksheet ID accepted")
	}
}
