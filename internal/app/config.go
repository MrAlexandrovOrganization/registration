package app

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DSN, Token, Listen, HTTP, TopicPrefix, TLSCert, TLSKey string
	Brokers                                                []string
	RootID, BotID                                          int64
	Milestones                                             []int
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
	for v := range strings.SplitSeq(os.Getenv("MILESTONES"), ",") {
		if v == "" {
			continue
		}
		n, e := strconv.Atoi(v)
		if e != nil || n <= 0 {
			return c, errors.New("invalid MILESTONES")
		}
		c.Milestones = append(c.Milestones, n)
	}
	return c, nil
}
