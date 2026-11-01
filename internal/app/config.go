package app

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DSN, Token, Listen, HTTP, TopicPrefix, TLSCert, TLSKey string
	Brokers                                                []string
	RootID, BotID                                          int64
	ParticipationEnabled                                   bool
	SheetsSpreadsheetID, SheetsCredentialsFile             string
	SheetsSheetID                                          int64
	SheetsInterval                                         time.Duration
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func Load() (Config, error) {
	c := Config{DSN: os.Getenv("DATABASE_URL"), Token: os.Getenv("BACKEND_TOKEN"), Listen: env("GRPC_LISTEN", ":50051"), HTTP: env("HTTP_LISTEN", ":9090"), TopicPrefix: env("KAFKA_TOPIC_PREFIX", "registration.telegram"), TLSCert: os.Getenv("GRPC_TLS_CERT"), TLSKey: os.Getenv("GRPC_TLS_KEY"), Brokers: strings.Split(env("KAFKA_BROKERS", "kafka:9092"), ",")}
	var err error
	c.SheetsSpreadsheetID = os.Getenv("GOOGLE_SHEETS_SPREADSHEET_ID")
	c.SheetsCredentialsFile = os.Getenv("GOOGLE_SHEETS_CREDENTIALS_FILE")
	if (c.SheetsSpreadsheetID == "") != (c.SheetsCredentialsFile == "") {
		return c, errors.New("configure both GOOGLE_SHEETS_SPREADSHEET_ID and GOOGLE_SHEETS_CREDENTIALS_FILE")
	}
	c.SheetsSheetID, err = strconv.ParseInt(env("GOOGLE_SHEETS_SHEET_ID", "0"), 10, 32)
	if err != nil || c.SheetsSheetID < 0 {
		return c, errors.New("invalid GOOGLE_SHEETS_SHEET_ID")
	}
	c.SheetsInterval, err = time.ParseDuration(env("GOOGLE_SHEETS_SYNC_INTERVAL", "1m"))
	if err != nil || c.SheetsInterval < 10*time.Second || c.SheetsInterval > 24*time.Hour {
		return c, errors.New("GOOGLE_SHEETS_SYNC_INTERVAL must be between 10s and 24h")
	}
	c.ParticipationEnabled, err = strconv.ParseBool(env("PARTICIPATION_ENABLED", "false"))
	if err != nil {
		return c, errors.New("invalid PARTICIPATION_ENABLED")
	}
	c.RootID, err = strconv.ParseInt(os.Getenv("ROOT_ID"), 10, 64)
	if err != nil || c.RootID <= 0 {
		return c, errors.New("ROOT_ID must be a positive integer")
	}
	c.BotID, err = strconv.ParseInt(os.Getenv("BOT_ID"), 10, 64)
	if err != nil || c.BotID <= 0 {
		return c, errors.New("BOT_ID must be a positive integer")
	}
	if c.DSN == "" || len(c.Token) < 32 {
		return c, errors.New("DATABASE_URL and BACKEND_TOKEN (32+ characters) are required")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return c, errors.New("both gRPC TLS files required")
	}
	if c.TLSCert == "" && env("ALLOW_INSECURE_GRPC", "false") != "true" {
		return c, errors.New("configure TLS or explicitly allow private-network plaintext gRPC")
	}
	return c, nil
}
