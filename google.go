package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	googleAuthURL    = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL   = "https://oauth2.googleapis.com/token"
	googleProfileURL = "https://gmail.googleapis.com/gmail/v1/users/me/profile"
	googleMailScope  = "https://mail.google.com/"
)

type googleOAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	HTTPClient   *http.Client
}

type googleTokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func loadGoogleOAuthConfig() (*googleOAuthConfig, error) {
	config := &googleOAuthConfig{
		ClientID:     strings.TrimSpace(os.Getenv("BESPOKE_MAIL_GOOGLE_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("BESPOKE_MAIL_GOOGLE_CLIENT_SECRET")),
		RedirectURL:  strings.TrimSpace(os.Getenv("BESPOKE_MAIL_GOOGLE_REDIRECT_URL")),
		HTTPClient:   &http.Client{Timeout: 20 * time.Second},
	}
	if config.ClientID == "" || config.ClientSecret == "" || config.RedirectURL == "" {
		return nil, errors.New("google OAuth is not configured")
	}
	redirect, err := url.Parse(config.RedirectURL)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" {
		return nil, errors.New("google OAuth redirect URL must be an absolute HTTPS URL")
	}
	return config, nil
}

func beginGoogleOAuth(ctx context.Context, sqldb *sql.DB, login string, config *googleOAuthConfig) (string, error) {
	if config == nil {
		return "", errors.New("google OAuth is not configured")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(state))
	if _, err := sqldb.ExecContext(ctx, `INSERT INTO oauth_states (state_hash, login, expires_at)
		VALUES (?, ?, datetime('now', '+10 minutes'))`, hash[:], login); err != nil {
		return "", err
	}
	values := url.Values{
		"client_id":     {config.ClientID},
		"redirect_uri":  {config.RedirectURL},
		"response_type": {"code"},
		"scope":         {googleMailScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return googleAuthURL + "?" + values.Encode(), nil
}

func finishGoogleOAuth(ctx context.Context, sqldb *sql.DB, vault *credentialVault, config *googleOAuthConfig, login, state, code string) (int64, error) {
	if vault == nil {
		return 0, errors.New("credential encryption is not configured")
	}
	if config == nil {
		return 0, errors.New("google OAuth is not configured")
	}
	if state == "" || code == "" {
		return 0, errors.New("google did not return a complete authorization")
	}
	hash := sha256.Sum256([]byte(state))
	var stateLogin string
	err := sqldb.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state_hash=?
		AND expires_at > datetime('now') RETURNING login`, hash[:]).Scan(&stateLogin)
	if errors.Is(err, sql.ErrNoRows) || stateLogin != login {
		return 0, errors.New("google authorization expired or did not match this user")
	}
	if err != nil {
		return 0, err
	}

	token, err := config.exchange(ctx, url.Values{
		"client_id":     {config.ClientID},
		"client_secret": {config.ClientSecret},
		"redirect_uri":  {config.RedirectURL},
		"grant_type":    {"authorization_code"},
		"code":          {code},
	})
	if err != nil {
		return 0, err
	}
	email, err := config.profileEmail(ctx, token.AccessToken)
	if err != nil {
		return 0, err
	}
	credential := accountCredential{
		AccessToken: token.AccessToken, RefreshToken: token.RefreshToken,
		TokenExpiry: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano),
	}
	ciphertext, nonce, err := vault.Seal(credential)
	if err != nil {
		return 0, err
	}
	var accountID int64
	err = sqldb.QueryRowContext(ctx, `INSERT INTO accounts
		(login, provider, email, credential_ciphertext, credential_nonce, status, status_detail)
		VALUES (?, 'gmail', ?, ?, ?, 'pending', 'Ready for first sync')
		ON CONFLICT(login, email) DO UPDATE SET credential_ciphertext=excluded.credential_ciphertext,
		credential_nonce=excluded.credential_nonce, status='pending', status_detail='Authorization renewed',
		updated_at=datetime('now') RETURNING id`, login, strings.ToLower(email), ciphertext, nonce).Scan(&accountID)
	return accountID, err
}

func (config *googleOAuthConfig) refresh(ctx context.Context, refreshToken string) (googleTokenResponse, error) {
	if config == nil || refreshToken == "" {
		return googleTokenResponse{}, errors.New("google authorization needs to be renewed")
	}
	return config.exchange(ctx, url.Values{
		"client_id": {config.ClientID}, "client_secret": {config.ClientSecret},
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken},
	})
}

func (config *googleOAuthConfig) exchange(ctx context.Context, values url.Values) (googleTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return googleTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := config.HTTPClient.Do(req)
	if err != nil {
		return googleTokenResponse{}, fmt.Errorf("contact Google authorization: %w", err)
	}
	defer resp.Body.Close()
	var token googleTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return googleTokenResponse{}, errors.New("decode Google authorization response")
	}
	if resp.StatusCode != http.StatusOK || token.Error != "" || token.AccessToken == "" {
		return googleTokenResponse{}, fmt.Errorf("google authorization failed: %s", strings.TrimSpace(token.Description))
	}
	return token, nil
}

func (config *googleOAuthConfig) profileEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleProfileURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := config.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("load Gmail profile: %w", err)
	}
	defer resp.Body.Close()
	var profile struct {
		Email string `json:"emailAddress"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&profile); err != nil {
		return "", errors.New("decode Gmail profile")
	}
	if resp.StatusCode != http.StatusOK || profile.Email == "" {
		return "", errors.New("google did not return a Gmail address")
	}
	return profile.Email, nil
}

// xoauth2Client implements Google's documented SASL XOAUTH2 initial response.
type xoauth2Client struct {
	username  string
	token     string
	responded bool
}

func (c *xoauth2Client) Start() (string, []byte, error) {
	response := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.username, c.token)
	return "XOAUTH2", []byte(response), nil
}

func (c *xoauth2Client) Next(_ []byte) ([]byte, error) {
	if c.responded {
		return nil, errors.New("xoauth2 authentication failed")
	}
	c.responded = true
	// Gmail sends a JSON error challenge and requires an empty response before
	// returning the tagged authentication failure.
	return []byte{}, nil
}
