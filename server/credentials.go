package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The Claude Code OAuth access token is the only credential that can read
// the usage endpoint — Anthropic does not expose it via API keys.
//
// Credential sources, in order:
//
//  1. CREDENTIALS_FILE (e.g. /data/credentials.json on fly.io) — where this
//     server persists tokens it has refreshed itself.
//  2. CLAUDE_CREDENTIALS env — the seed credential set as a deploy secret,
//     produced by `go run ./cmd/login` (a dedicated OAuth session).
//  3. macOS Keychain ("Claude Code-credentials") — local dev.
//  4. ~/.claude/.credentials.json — Claude Code's store on other platforms.
//
// Refresh policy: sources 1–2 are a session dedicated to this server, so it
// refreshes them itself (and persists the rotated tokens to
// CREDENTIALS_FILE). Sources 3–4 belong to Claude Code — Anthropic rotates
// refresh tokens on use, so refreshing those here would invalidate Claude
// Code's copy and break its login. For those the server only re-reads the
// store and tells the user to run `claude` if the token has expired.

const (
	keychainService = "Claude Code-credentials"
	oauthClientID   = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthTokenURL   = "https://console.anthropic.com/v1/oauth/token"
)

type oauthCreds struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    int64  `json:"expiresAt"` // unix millis
}

func (c oauthCreds) expired() bool {
	// A minute of slack so we never hand out a token that dies mid-request.
	return time.Now().UnixMilli() >= c.ExpiresAt-time.Minute.Milliseconds()
}

var credsMu sync.Mutex
var cachedCreds oauthCreds
var forceRefresh bool // set after an upstream 401 on an owned credential

// accessToken returns a valid access token, re-reading the credential store
// and refreshing (for owned credentials) as needed.
func accessToken() (string, error) {
	credsMu.Lock()
	defer credsMu.Unlock()
	if cachedCreds.AccessToken != "" && !cachedCreds.expired() && !forceRefresh {
		return cachedCreds.AccessToken, nil
	}

	creds, owned, err := readCredentials()
	if err != nil {
		return "", err
	}
	if creds.expired() || (forceRefresh && owned) {
		if !owned {
			return "", errors.New("stored Claude Code token is expired — run `claude` once so it refreshes, then retry")
		}
		if creds.RefreshToken == "" {
			return "", errors.New("credential is expired and has no refresh token — rerun `go run ./cmd/login` and update the CLAUDE_CREDENTIALS secret")
		}
		creds, err = refreshCredentials(creds)
		if err != nil {
			return "", err
		}
		persistCredentials(creds)
	}
	forceRefresh = false
	cachedCreds = creds
	return creds.AccessToken, nil
}

// invalidateToken drops the cached token so the next request re-reads the
// store, and (for owned credentials) forces a refresh. Called on an
// upstream 401.
func invalidateToken() {
	credsMu.Lock()
	cachedCreds = oauthCreds{}
	forceRefresh = true
	credsMu.Unlock()
}

// readCredentials returns the parsed credentials and whether this server
// owns them (may refresh them) or they belong to Claude Code.
func readCredentials() (oauthCreds, bool, error) {
	if path := os.Getenv("CREDENTIALS_FILE"); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			creds, err := parseCredentials(raw)
			return creds, true, err
		}
	}
	if env := os.Getenv("CLAUDE_CREDENTIALS"); env != "" {
		creds, err := parseCredentials([]byte(env))
		return creds, true, err
	}
	if raw, err := readKeychain(); err == nil {
		creds, err := parseCredentials(raw)
		return creds, false, err
	}
	if raw, err := readCredentialsFile(); err == nil {
		creds, err := parseCredentials(raw)
		return creds, false, err
	}
	return oauthCreds{}, false, errors.New("no Claude credentials found: set CLAUDE_CREDENTIALS (from `go run ./cmd/login`), or log in to Claude Code on this machine")
}

func parseCredentials(raw []byte) (oauthCreds, error) {
	var wrapper struct {
		ClaudeAiOauth oauthCreds `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return oauthCreds{}, fmt.Errorf("parse credentials: %w", err)
	}
	if wrapper.ClaudeAiOauth.AccessToken == "" {
		return oauthCreds{}, errors.New("credentials found but claudeAiOauth.accessToken is empty")
	}
	return wrapper.ClaudeAiOauth, nil
}

// refreshCredentials exchanges the refresh token for a new token pair.
func refreshCredentials(creds oauthCreds) (oauthCreds, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": creds.RefreshToken,
		"client_id":     oauthClientID,
	})
	resp, err := httpClient.Post(oauthTokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return oauthCreds{}, fmt.Errorf("token refresh: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return oauthCreds{}, fmt.Errorf("token refresh returned %d: %s — rerun `go run ./cmd/login` and update the CLAUDE_CREDENTIALS secret", resp.StatusCode, truncate(string(respBody), 200))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return oauthCreds{}, fmt.Errorf("parse refresh response: %w", err)
	}
	next := oauthCreds{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UnixMilli(),
	}
	if next.RefreshToken == "" { // provider kept the old one
		next.RefreshToken = creds.RefreshToken
	}
	slog.Info("refreshed oauth token", "expires_at", time.UnixMilli(next.ExpiresAt))
	return next, nil
}

// persistCredentials writes rotated tokens to CREDENTIALS_FILE so they
// survive a restart (the deploy secret's refresh token is single-use once
// rotation happens). Best-effort: without the file the server still works
// until the next restart.
func persistCredentials(creds oauthCreds) {
	path := os.Getenv("CREDENTIALS_FILE")
	if path == "" {
		return
	}
	raw, _ := json.Marshal(map[string]oauthCreds{"claudeAiOauth": creds})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		slog.Error("persist credentials", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Error("persist credentials", "err", err)
	}
}

func readKeychain() ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("keychain read: %w", err)
	}
	return []byte(strings.TrimSpace(string(out))), nil
}

func readCredentialsFile() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
}
