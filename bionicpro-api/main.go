package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	Port          string `envconfig:"PORT" required:"true"`
	ClkAddr       string `envconfig:"CLICKHOUSE_ADDR" required:"true"`
	ClkDB         string `envconfig:"CLICKHOUSE_DATABASE" required:"true"`
	ClkUsername   string `envconfig:"CLICKHOUSE_USERNAME" required:"true"`
	ClkPassword   string `envconfig:"CLICKHOUSE_PASSWORD" required:"true"`
	OIDCIssuer    string `envconfig:"OIDC_ISSUER" required:"true"`
	OIDCPubIssuer string `envconfig:"OIDC_PUBLIC_ISSUER" required:"true"`
}

type Principal struct {
	UserID   string
	Username string
	Email    string
	Roles    []string
}

type principalKey struct{}

func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

var errNoReport = errors.New("no report found")

func main() {
	ctx := context.Background()

	var cfg Config
	envconfig.MustProcess("", &cfg)

	dbConn, err := connectDB(ctx, cfg.ClkAddr, cfg.ClkDB, cfg.ClkUsername, cfg.ClkPassword)
	if err != nil {
		slog.Error("failed to connect to the DB", "error", err)
		os.Exit(1)
	}
	defer dbConn.Close()

	provider, err := oidc.NewProvider(oidc.InsecureIssuerURLContext(ctx, cfg.OIDCPubIssuer), cfg.OIDCIssuer)
	if err != nil {
		slog.Error("failed to instantiate OIDC provider", "error", err)
		os.Exit(1)
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID:             "reports-api",
		SupportedSigningAlgs: []string{oidc.RS256},
	})

	http.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		principal, ok := principalFrom(ctx)
		if !ok {
			unauthorized(w)
			return
		}

		slog.Info("authorized report request", "principal_id", principal.UserID, "username", principal.Username)

		reportDate, err := parseDate(r, "date")
		if err != nil {
			http.Error(w, "bad date value", http.StatusBadRequest)
			return
		}

		report, err := fetchReport(ctx, dbConn, principal.Email, reportDate)
		if err != nil {
			if errors.Is(err, errNoReport) {
				http.NotFound(w, r)
				return
			}

			slog.Error("failed to fetch report for user", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if err := enc.Encode(report); err != nil {
			slog.Error("failed to send report to the client", "error", err)
		}
	})

	err = http.ListenAndServe(fmt.Sprintf(":%s", cfg.Port), requireBearer(verifier, http.DefaultServeMux))
	if !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server failed", "error", err)
		os.Exit(1)
	}
}

func requireBearer(v *oidc.IDTokenVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
			slog.Info("no bearer token in request")
			unauthorized(w)
			return
		}

		tok, err := v.Verify(r.Context(), raw)
		if err != nil {
			slog.Info("token rejected", "error", err)
			unauthorized(w)
			return
		}

		var c struct {
			Typ               string `json:"typ"`
			PreferredUsername string `json:"preferred_username"`
			Email             string `json:"email"`
			RealmAccess       struct {
				Roles []string `json:"roles"`
			} `json:"realm_access"`
		}

		if err := tok.Claims(&c); err != nil || c.Typ != "Bearer" {
			slog.Info("unexpected token", "error", err, "type", c.Typ)
			unauthorized(w)
			return
		}

		p := Principal{
			UserID:   tok.Subject,
			Username: c.PreferredUsername,
			Email:    c.Email,
			Roles:    c.RealmAccess.Roles,
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func parseDate(r *http.Request, fieldName string) (time.Time, error) {
	val := r.URL.Query().Get(fieldName)
	if val == "" {
		return time.Time{}, fmt.Errorf("field %q is missing", fieldName)
	}

	ret, err := time.Parse(time.DateOnly, val)
	if err != nil {
		return time.Time{}, err
	}

	return ret, nil
}

func connectDB(ctx context.Context, addr, db, username, password string) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{
			addr,
		},
		Auth: clickhouse.Auth{
			Database: db,
			Username: username,
			Password: password,
		},
		ClientInfo: clickhouse.ClientInfo{
			Products: []struct {
				Name    string
				Version string
			}{
				{Name: "an-example-go-client", Version: "0.1"},
			},
		},
		Debugf: func(format string, v ...interface{}) {
			fmt.Printf(format, v...)
		},
	})

	if err != nil {
		return nil, err
	}

	if err := conn.Ping(ctx); err != nil {
		if exception, ok := err.(*clickhouse.Exception); ok {
			fmt.Printf("Exception [%d] %s \n%s\n", exception.Code, exception.Message, exception.StackTrace)
		}
		return nil, err
	}
	return conn, nil
}

func fetchReport(ctx context.Context, dbConn driver.Conn, user string, date time.Time) (json.RawMessage, error) {
	type reportRow struct {
		Date        time.Time `ch:"report_date" json:"date"`
		EventsCount uint64    `ch:"events_count" json:"events_count"`
		AvgValue    float64   `ch:"avg_value" json:"avg_value"`
		MinValue    float64   `ch:"min_value" json:"min_value"`
		MaxValue    float64   `ch:"max_value" json:"max_value"`
	}

	query := `
SELECT report_date, events_count, avg_value, min_value, max_value
FROM client_telemetry_mart
WHERE email = ?
AND report_date = toDate(?)
`

	rows := []reportRow{}
	if err := dbConn.Select(ctx, &rows, query, user, date.Format(time.DateOnly)); err != nil {
		return nil, fmt.Errorf("select report: %w", err)
	}

	if len(rows) == 0 {
		return nil, errNoReport
	}

	return json.Marshal(rows)
}
