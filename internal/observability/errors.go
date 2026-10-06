package observability

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrorAttrs intentionally never formats err: driver and network errors can
// embed credentials, SQL parameters, addresses and request data.
func ErrorAttrs(err error) []slog.Attr {
	class := "internal"
	var pg *pgconn.PgError
	var network net.Error
	switch {
	case errors.Is(err, context.Canceled):
		class = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		class = "deadline"
	case errors.As(err, &pg):
		class = "database"
		if validSQLState(pg.Code) {
			return []slog.Attr{slog.String("error_class", class), slog.String("sqlstate", pg.Code)}
		}
	case errors.As(err, &network):
		class = "network"
	}
	return []slog.Attr{slog.String("error_class", class)}
}

func validSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for _, c := range code {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}
