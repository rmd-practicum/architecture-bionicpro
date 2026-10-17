package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/kelseyhightower/envconfig"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Config struct {
	Port          string        `envconfig:"PORT" required:"true"`
	ClkAddr       string        `envconfig:"CLICKHOUSE_ADDR" required:"true"`
	ClkDB         string        `envconfig:"CLICKHOUSE_DATABASE" required:"true"`
	ClkUsername   string        `envconfig:"CLICKHOUSE_USERNAME" required:"true"`
	ClkPassword   string        `envconfig:"CLICKHOUSE_PASSWORD" required:"true"`
	OIDCIssuer    string        `envconfig:"OIDC_ISSUER" required:"true"`
	OIDCPubIssuer string        `envconfig:"OIDC_PUBLIC_ISSUER" required:"true"`
	S3Endpoint    string        `envconfig:"S3_ENDPOINT" required:"true"`
	S3AccessKey   string        `envconfig:"S3_ACCESS_KEY" required:"true"`
	S3SecretKey   string        `envconfig:"S3_SECRET_KEY" required:"true"`
	S3Bucket      string        `envconfig:"S3_BUCKET" required:"true"`
	CDNURL        string        `envconfig:"CDN_URL" required:"true"`
	CDNSecret     string        `envconfig:"CDN_LINK_SECRET" required:"true"`
	CDNLinkTTL    time.Duration `envconfig:"CDN_LINK_TTL" default:"5m"`
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

	s3, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	})
	if err != nil {
		slog.Error("failed to create S3 client", "error", err)
		os.Exit(1)
	}

	if err := ensureBucket(ctx, s3, cfg.S3Bucket); err != nil {
		slog.Error("failed to prepare S3 bucket", "error", err)
		os.Exit(1)
	}

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

		key := reportDate.Format(time.DateOnly) + "/" + principal.UserID + ".json"

		_, err = s3.StatObject(ctx, cfg.S3Bucket, key, minio.StatObjectOptions{})
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			err = storeReport(ctx, dbConn, s3, cfg.S3Bucket, key, principal.Email, reportDate)
		}

		if err != nil {
			if errors.Is(err, errNoReport) {
				http.NotFound(w, r)
				return
			}

			slog.Error("failed to fetch report for user", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		exp := time.Now().Add(cfg.CDNLinkTTL)
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		err = enc.Encode(map[string]any{
			"url":        signLink(cfg.CDNURL, cfg.CDNSecret, "/"+cfg.S3Bucket+"/"+key, exp),
			"expires_at": exp.UTC(),
		})
		if err != nil {
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

func storeReport(ctx context.Context, dbConn driver.Conn, s3 *minio.Client, bucket, key, user string, date time.Time) error {
	report, err := fetchReport(ctx, dbConn, user, date)
	if err != nil {
		return fmt.Errorf("failed to fetch report: %w", err)
	}

	_, err = s3.PutObject(ctx, bucket, key, bytes.NewReader(report), int64(len(report)), minio.PutObjectOptions{ContentType: "application/json"})
	if err != nil {
		return fmt.Errorf("failed to store report in s3: %w", err)
	}

	return nil
}

func signLink(base, secret, path string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	sum := md5.Sum([]byte(e + path + " " + secret))
	return base + path + "?md5=" + base64.RawURLEncoding.EncodeToString(sum[:]) + "&expires=" + e
}

func ensureBucket(ctx context.Context, s3 *minio.Client, bucket string) error {
	exists, err := s3.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("failed to check if bucket exists: %w", err)
	}

	if !exists {
		if err := s3.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("faield to make bucket: %w", err)
		}
	}

	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/*"]}]}`, bucket)
	if err := s3.SetBucketPolicy(ctx, bucket, policy); err != nil {
		return fmt.Errorf("failed to set bucket policy: %w", err)
	}

	return nil
}
