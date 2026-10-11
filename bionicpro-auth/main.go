package main

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"uuid"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"
	"github.com/kelseyhightower/envconfig"
	"github.com/zitadel/oidc/v3/pkg/client/rp"
	httphelper "github.com/zitadel/oidc/v3/pkg/http"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"golang.org/x/sync/singleflight"
)

type Config struct {
	Port         string   `envconfig:"PORT" required:"true"`
	ClientID     string   `envconfig:"CLIENT_ID" required:"true"`
	ClientSecret string   `envconfig:"CLIENT_SECRET"`
	Issuer       string   `envconfig:"ISSUER" required:"true"`
	Scopes       []string `envconfig:"SCOPES" default:"openid"`
	FrontendURL  string   `envconfig:"FRONTEND_URL" required:"true"`
	DiscoveryURL string   `envconfig:"DISCOVERY_URL"`
}

type User struct {
	Sub      string   `json:"sub"`
	Username string   `json:"username"`
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Roles    []string `json:"roles"`
}

type Tokens struct {
	Access  string
	Refresh string
	IDToken string
	Expiry  time.Time
}

type graceStore struct {
	scs.Store
	grace time.Duration
}

func (s graceStore) Delete(token string) error {
	b, found, err := s.Store.Find(token)
	if err != nil || !found {
		return err
	}
	return s.Store.Commit(token, b, time.Now().Add(s.grace))
}

type accessTokenKey struct{}

const cookieHashKey = "test1234test1234"
const cookieEncKey = "test1234test1234"
const callbackPath = "/auth/callback"

func main() {
	ctx := context.Background()

	gob.Register(User{})
	gob.Register(Tokens{})

	var cfg Config
	envconfig.MustProcess("", &cfg)

	client := &http.Client{
		Timeout: time.Minute,
	}

	cookieHandler := httphelper.NewCookieHandler([]byte(cookieHashKey), []byte(cookieEncKey))

	sessionMgr := scs.New()
	store := graceStore{
		Store: memstore.New(),
		grace: 10 * time.Second,
	}
	sessionMgr.Store = store
	sessionMgr.IdleTimeout = 30 * time.Minute
	sessionMgr.Lifetime = 10 * time.Hour
	sessionMgr.Cookie.Secure = true
	sessionMgr.Cookie.Name = "bionicpro-auth-session"

	options := []rp.Option{
		rp.WithCookieHandler(cookieHandler),
		rp.WithVerifierOpts(rp.WithIssuedAtOffset(5 * time.Second)),
		rp.WithHTTPClient(client),
		rp.WithSigningAlgsFromDiscovery(),
		rp.WithPKCE(cookieHandler),
		rp.WithCustomDiscoveryUrl(cfg.DiscoveryURL),
	}

	publicURL := fmt.Sprintf("http://localhost:%s", cfg.Port)
	redirectURI := fmt.Sprintf("%s%s", publicURL, callbackPath)
	provider, err := rp.NewRelyingPartyOIDC(ctx, cfg.Issuer, cfg.ClientID, cfg.ClientSecret, redirectURI, cfg.Scopes, options...)
	if err != nil {
		slog.Error("failed to create OIDC provider", "error", err)
		os.Exit(1)
	}

	state := func() string {
		return uuid.New().String()
	}

	login := rp.AuthURLHandler(state, provider)

	var refreshLock singleflight.Group
	protected := func(next http.HandlerFunc) http.Handler {
		return newProtectedMiddleware(&refreshLock, sessionMgr, provider, next)
	}

	http.HandleFunc("GET /auth/login", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		sessionMgr.Put(ctx, "returnTo", safeReturnTo(r.URL.Query().Get("returnTo")))
		login(w, r)
	})
	http.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		tokens, _ := sessionMgr.Get(ctx, "tokens").(Tokens)
		sessionToken := sessionMgr.Token(ctx)
		_ = sessionMgr.Destroy(ctx)
		_ = store.Store.Delete(sessionToken)

		if tokens.Refresh != "" {
			oauthCfg := provider.OAuthConfig()

			form := url.Values{
				"client_id":     {oauthCfg.ClientID},
				"client_secret": {oauthCfg.ClientSecret},
				"refresh_token": {tokens.Refresh},
			}

			logoutURL := strings.TrimSuffix(oauthCfg.Endpoint.TokenURL, "/token") + "/logout"

			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, logoutURL, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			resp, err := provider.HttpClient().Do(req)
			if err != nil {
				slog.WarnContext(ctx, "keycloak logout failed", "error", err)
			} else {
				resp.Body.Close()
				if resp.StatusCode >= 300 {
					slog.WarnContext(ctx, "keycloak logout rejected", "status", resp.StatusCode)
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	http.Handle("GET /auth/me", protected(func(w http.ResponseWriter, r *http.Request) {
		user, _ := sessionMgr.Get(r.Context(), "user").(User)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(user)
	}))
	http.Handle(fmt.Sprintf("GET %s", callbackPath), rp.CodeExchangeHandler(func(w http.ResponseWriter, r *http.Request, tokens *oidc.Tokens[*oidc.IDTokenClaims], state string, rp rp.RelyingParty) {
		ctx := r.Context()

		if err := sessionMgr.RenewToken(ctx); err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}

		claims := tokens.IDTokenClaims

		sessionMgr.Put(ctx, "user", User{
			Sub:      claims.Subject,
			Username: claims.PreferredUsername,
			Name:     claims.Name,
			Email:    claims.Email,
			Roles:    realmRoles(claims.Claims),
		})
		sessionMgr.Put(ctx, "tokens", Tokens{
			Access:  tokens.AccessToken,
			Refresh: tokens.RefreshToken,
			IDToken: tokens.IDToken,
			Expiry:  tokens.Expiry,
		})

		http.Redirect(w, r, cfg.FrontendURL+safeReturnTo(sessionMgr.PopString(ctx, "returnTo")), http.StatusFound)
	}, provider))

	err = http.ListenAndServe(fmt.Sprintf(":%s", cfg.Port), withCORS(cfg.FrontendURL, sessionMgr.LoadAndSave(http.DefaultServeMux)))
	if !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server failed", "error", err)
		os.Exit(1)
	}
}

func accessToken(ctx context.Context, lock *singleflight.Group, sessionMgr *scs.SessionManager, provider rp.RelyingParty) (string, error) {
	tokens, ok := sessionMgr.Get(ctx, "tokens").(Tokens)
	if !ok {
		return "", fmt.Errorf("tokens not set")
	}

	if time.Until(tokens.Expiry) > 30*time.Second {
		return tokens.Access, nil
	}

	rawTokens, err, _ := lock.Do(sessionMgr.Token(ctx), func() (any, error) {
		return rp.RefreshTokens[*oidc.IDTokenClaims](ctx, provider, tokens.Refresh, "", "")
	})
	if err != nil {
		_ = sessionMgr.Destroy(ctx)
		return "", fmt.Errorf("failed to refresh tokens: %w", err)
	}

	newTokens := rawTokens.(*oidc.Tokens[*oidc.IDTokenClaims])

	tokens.Access, tokens.Expiry = newTokens.AccessToken, newTokens.Expiry

	if newTokens.RefreshToken != "" {
		tokens.Refresh = newTokens.RefreshToken
	}

	if newTokens.IDToken != "" {
		tokens.IDToken = newTokens.IDToken
	}

	sessionMgr.Put(ctx, "tokens", tokens)

	return tokens.Access, nil
}

func newProtectedMiddleware(lock *singleflight.Group, sessionMgr *scs.SessionManager, provider rp.RelyingParty, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		token, err := accessToken(ctx, lock, sessionMgr, provider)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if err := sessionMgr.RenewToken(ctx); err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}

		next(w, r.WithContext(context.WithValue(ctx, accessTokenKey{}, token)))
	})
}

func withCORS(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		next.ServeHTTP(w, r)
	})
}

func realmRoles(claims map[string]any) []string {
	realmAccess, _ := claims["realm_access"].(map[string]any)
	rawRoles, _ := realmAccess["roles"].([]any)

	roles := make([]string, 0, len(rawRoles))

	for _, rawRole := range rawRoles {
		if s, ok := rawRole.(string); ok {
			roles = append(roles, s)
		}
	}

	return roles
}

func safeReturnTo(raw string) string {
	const fallback = "/"

	if raw == "" || len(raw) > 2048 {
		return fallback
	}

	parsedURL, err := url.Parse(raw)
	if err != nil || parsedURL.Scheme != "" || parsedURL.Host != "" || parsedURL.User != nil {
		return fallback
	}

	return raw
}
