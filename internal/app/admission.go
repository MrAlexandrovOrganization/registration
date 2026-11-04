package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strconv"

	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

func closeAdmission(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("close-admission", flag.ContinueOnError)
	question := flags.Int64("question", 0, "source agreement ID of the active campaign")
	if err := flags.Parse(args); err != nil {
		return err
	}
	root, err := strconv.ParseInt(os.Getenv("ROOT_ID"), 10, 64)
	if *question <= 0 || flags.NArg() != 0 || err != nil || root <= 0 {
		return errors.New("close-admission requires --question and configured ROOT_ID")
	}
	db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err = store.CheckSchema(ctx, db); err != nil {
		return err
	}
	svc := &service.Service{DB: db, RootID: root}
	result, err := svc.CloseFinalAdmission(ctx, *question)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
