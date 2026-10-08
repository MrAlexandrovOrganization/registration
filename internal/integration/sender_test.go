//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	pb "registration.local/backend/api"
	"registration.local/backend/internal/app"
	"registration.local/backend/internal/service"
	"registration.local/backend/internal/store"
)

func TestSenderHandoff(t *testing.T) {
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
	_, err = bootstrap.Exec(ctx, "CREATE SCHEMA sender_test")
	must(err)
	cfg.ConnConfig.RuntimeParams["search_path"] = "sender_test"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer db.Close()
	must(store.Migrate(ctx, db))
	svc := &service.Service{DB: db, BotID: 1000, RootID: 101}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(app.Auth(strings.Repeat("x", 32))))
	pb.RegisterRegistrationServer(server, svc)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	must(err)
	defer conn.Close()
	client := pb.NewRegistrationClient(conn)
	auth := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.Repeat("x", 32))
	_, err = client.ReleaseSender(ctx, &pb.ReleaseSenderRequest{Worker: "old-worker"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal("unauthenticated release accepted")
	}
	for _, worker := range []string{"", "short", strings.Repeat("x", 101)} {
		_, err = client.ReleaseSender(auth, &pb.ReleaseSenderRequest{Worker: worker})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid identity accepted")
		}
	}
	release := func(worker string, want bool) {
		t.Helper()
		r, err := client.ReleaseSender(auth, &pb.ReleaseSenderRequest{Worker: worker})
		must(err)
		if r.Released != want {
			t.Fatalf("release %s = %v, want %v", worker, r.Released, want)
		}
	}
	receipt, err := client.Accept(auth, &pb.Update{Id: 1, Actor: 42, Chat: 42, ChatType: "private", Text: "/start"})
	must(err)
	if len(receipt.DeliveryIds) == 0 {
		t.Fatal("missing delivery")
	}
	delivery, err := client.Claim(auth, &pb.ClaimRequest{Id: receipt.DeliveryIds[0], Worker: "old-worker"})
	must(err)
	if delivery.Id == 0 {
		t.Fatal("delivery was not claimed")
	}
	release("other-worker", false)
	_, err = client.Claim(auth, &pb.ClaimRequest{Worker: "new-worker"})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatal("foreign release removed active lease")
	}
	_, err = db.Exec(ctx, "UPDATE runtime_state SET cooldown_until=now()+interval '1 hour' WHERE id=1")
	must(err)
	var cooldown, leaseUntil time.Time
	must(db.QueryRow(ctx, "SELECT cooldown_until FROM runtime_state WHERE id=1").Scan(&cooldown))
	must(db.QueryRow(ctx, "SELECT lease_until FROM outbound_messages WHERE id=$1", delivery.Id).Scan(&leaseUntil))
	release("old-worker", true)
	release("old-worker", false) // Idempotent even if the first response was lost.
	newClaim, err := client.Claim(auth, &pb.ClaimRequest{Worker: "new-worker"})
	must(err) // No 90-second wait.
	if newClaim.NotBeforeUnixMs <= time.Now().UnixMilli() {
		t.Fatal("handoff lost cooldown")
	}
	release("old-worker", false) // Late cleanup cannot release the successor.
	_, err = client.Claim(auth, &pb.ClaimRequest{Worker: "third-worker"})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatal("late release removed successor lease")
	}
	var gotCooldown, gotLeaseUntil time.Time
	var jobStatus, jobLease string
	must(db.QueryRow(ctx, "SELECT cooldown_until FROM runtime_state WHERE id=1").Scan(&gotCooldown))
	must(db.QueryRow(ctx, "SELECT status,lease,lease_until FROM outbound_messages WHERE id=$1", delivery.Id).Scan(&jobStatus, &jobLease, &gotLeaseUntil))
	if !gotCooldown.Equal(cooldown) || !gotLeaseUntil.Equal(leaseUntil) || jobStatus != "sending" || jobLease != delivery.Lease {
		t.Fatal("release changed cooldown or in-flight delivery")
	}
	// Crash recovery remains possible without ReleaseSender.
	_, err = db.Exec(ctx, "UPDATE runtime_state SET sender_until=now()-interval '1 second' WHERE id=1")
	must(err)
	_, err = client.Claim(auth, &pb.ClaimRequest{Worker: "third-worker"})
	must(err)
	release("new-worker", false)
	release("third-worker", true)
}
