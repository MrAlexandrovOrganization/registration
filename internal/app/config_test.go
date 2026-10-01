package app

import (
	"strings"
	"testing"
)

func TestLoadWithoutRegistrationSwitches(t *testing.T) {
	for key, value := range map[string]string{
		"DATABASE_URL":  "postgres://fixture:fixture@localhost/registration_test",
		"BACKEND_TOKEN": strings.Repeat("x", 32),
		"ROOT_ID":       "1001", "BOT_ID": "1000",
		"ALLOW_INSECURE_GRPC": "true", "GRPC_TLS_CERT": "", "GRPC_TLS_KEY": "",
		"MILESTONES": "25,50",
		// Obsolete settings must not affect startup, even if left in the environment.
		"REGISTRATION_EPOCH": "unused", "REGISTRATION_OPEN": "unused", "DELIVERY_ENABLED": "unused",
	} {
		t.Setenv(key, value)
	}
	c, err := Load()
	if err != nil || c.RootID != 1001 || c.BotID != 1000 || len(c.Milestones) != 2 {
		t.Fatalf("configuration failed: %v", err)
	}
}
