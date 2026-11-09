package audiocom

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrNotConnected = errors.New("audio.com is not connected: open GET /api/v1/audiocom/connect in a browser and sign in")

// discovery is RFC 8414 metadata from /.well-known/oauth-authorization-server.
type discovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// OAuth handles client registration, the PKCE authorization flow and token
// refresh. Tokens live in the audiocom_auth table and never leave the server.
type OAuth struct {
	DB          *gorm.DB
	APIBase     string // https://api.audio.com
	RedirectURI string // our /api/v1/audiocom/callback
	http        *http.Client

	mu   sync.Mutex // serializes refreshes
	disc *discovery
}

func (o *OAuth) discover(ctx context.Context) (*discovery, error) {
	if o.disc != nil {
		return o.disc, nil
	}
	var d discovery
	if err := o.getJSON(ctx, o.APIBase+"/.well-known/oauth-authorization-server", &d); err != nil {
		return nil, fmt.Errorf("oauth discovery: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, errors.New("oauth discovery: missing endpoints")
	}
	o.disc = &d
	return &d, nil
}

func (o *OAuth) load() (Auth, error) {
	a := Auth{Name: "default"}
	err := o.DB.Where("name = ?", "default").Limit(1).Find(&a).Error
	return a, err
}

func (o *OAuth) save(a *Auth) error {
	a.Name = "default"
	return o.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(a).Error
}

// Connected reports whether a token (or refresh token) is stored.
func (o *OAuth) Connected() bool {
	a, err := o.load()
	return err == nil && (a.RefreshToken != "" || a.AccessToken != "")
}

// AuthorizeURL registers a client if needed (dynamic registration) and
// returns the URL the user must open to grant access.
func (o *OAuth) AuthorizeURL(ctx context.Context) (string, error) {
	d, err := o.discover(ctx)
	if err != nil {
		return "", err
	}
	a, err := o.load()
	if err != nil {
		return "", err
	}
	if a.ClientID == "" || a.RedirectURI != o.RedirectURI {
		if d.RegistrationEndpoint == "" {
			return "", errors.New("audio.com offers no client registration endpoint")
		}
		if err := o.register(ctx, d.RegistrationEndpoint, &a); err != nil {
			return "", err
		}
	}
	a.PendingState, a.PendingVerifier = randomString(24), randomString(48)
	if err := o.save(&a); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(a.PendingVerifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.ClientID},
		"redirect_uri":          {o.RedirectURI},
		"scope":                 {"public"},
		"state":                 {a.PendingState},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	return d.AuthorizationEndpoint + "?" + q.Encode(), nil
}

func (o *OAuth) register(ctx context.Context, endpoint string, a *Auth) error {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "howwebiz-api",
		"redirect_uris":              []string{o.RedirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      "public",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := o.doJSON(req, &out); err != nil {
		return fmt.Errorf("client registration: %w", err)
	}
	if out.ClientID == "" {
		return errors.New("client registration: no client_id returned")
	}
	a.ClientID, a.ClientSecret, a.RedirectURI = out.ClientID, out.ClientSecret, o.RedirectURI
	return nil
}

// Callback completes the authorization: checks state, exchanges the code.
func (o *OAuth) Callback(ctx context.Context, code, state string) error {
	a, err := o.load()
	if err != nil {
		return err
	}
	if a.PendingState == "" || state != a.PendingState {
		return errors.New("invalid or expired state; start again at /api/v1/audiocom/connect")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {a.RedirectURI},
		"code_verifier": {a.PendingVerifier},
	}
	if err := o.token(ctx, &a, form); err != nil {
		return err
	}
	now := time.Now().UTC()
	a.ConnectedAt, a.PendingState, a.PendingVerifier = &now, "", ""
	return o.save(&a)
}

// Token returns a valid access token, refreshing it if it expires soon.
// force refreshes regardless (after a 401).
func (o *OAuth) Token(ctx context.Context, force bool) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	a, err := o.load()
	if err != nil {
		return "", err
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return "", ErrNotConnected
	}
	fresh := a.ExpiresAt == nil || time.Until(*a.ExpiresAt) > time.Minute
	if a.AccessToken != "" && fresh && !force {
		return a.AccessToken, nil
	}
	if a.RefreshToken == "" {
		return "", ErrNotConnected
	}
	if err := o.token(ctx, &a, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.RefreshToken}}); err != nil {
		return "", fmt.Errorf("refresh token: %w (reconnect at /api/v1/audiocom/connect)", err)
	}
	return a.AccessToken, o.save(&a)
}

func (o *OAuth) token(ctx context.Context, a *Auth, form url.Values) error {
	d, err := o.discover(ctx)
	if err != nil {
		return err
	}
	form.Set("client_id", a.ClientID)
	if a.ClientSecret != "" {
		form.Set("client_secret", a.ClientSecret)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := o.doJSON(req, &out); err != nil {
		return err
	}
	if out.AccessToken == "" {
		return errors.New("token endpoint returned no access_token")
	}
	a.AccessToken = out.AccessToken
	if out.RefreshToken != "" { // rotation: keep the newest
		a.RefreshToken = out.RefreshToken
	}
	a.ExpiresAt = nil
	if out.ExpiresIn > 0 {
		t := time.Now().UTC().Add(time.Duration(out.ExpiresIn) * time.Second)
		a.ExpiresAt = &t
	}
	return nil
}

// Disconnect forgets the tokens (the registered client is kept).
func (o *OAuth) Disconnect() error {
	return o.DB.Model(&Auth{}).Where("name = ?", "default").Updates(map[string]any{
		"access_token": "", "refresh_token": "", "expires_at": nil, "connected_at": nil,
	}).Error
}

func (o *OAuth) getJSON(ctx context.Context, u string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	return o.doJSON(req, v)
}

func (o *OAuth) doJSON(req *http.Request, v any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
			Message     string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s %s: %d %s%s%s", req.Method, req.URL.Path, resp.StatusCode, e.Error, e.Description, e.Message)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}
