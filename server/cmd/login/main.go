// Command login performs the Claude OAuth PKCE flow and prints a
// credentials JSON blob for the CLAUDE_CREDENTIALS deploy secret.
//
// This creates a NEW session dedicated to the server — it does not touch
// the credentials Claude Code keeps in the Keychain, so the server can
// refresh its tokens freely without breaking the local Claude Code login.
//
// Usage:
//
//	go run ./cmd/login
//	  1. open the printed URL in a browser (logged in to claude.ai)
//	  2. approve, copy the code shown on the callback page
//	  3. paste it back here
//	  4. pipe/copy the JSON on stdout into:
//	     fly secrets set CLAUDE_CREDENTIALS='<json>'
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	clientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	authorizeURL = "https://claude.ai/oauth/authorize"
	tokenURL    = "https://console.anthropic.com/v1/oauth/token"
	redirectURI = "https://console.anthropic.com/oauth/code/callback"
	scopes      = "org:create_api_key user:profile user:inference"
)

func main() {
	verifier := randomURLSafe(32)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{
		"code":                  {"true"},
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {scopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {verifier},
	}
	fmt.Fprintf(os.Stderr, "Open this URL in a browser logged in to claude.ai:\n\n%s?%s\n\nPaste the code shown after approving (looks like xxxx#yyyy): ", authorizeURL, q.Encode())

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fatal("read code: %v", err)
	}
	code, state, _ := strings.Cut(strings.TrimSpace(line), "#")
	if state == "" {
		state = verifier
	}

	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         state,
		"client_id":     clientID,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	resp, err := http.Post(tokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		fatal("token exchange: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		fatal("token exchange returned %d: %s", resp.StatusCode, respBody)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		fatal("parse token response: %v", err)
	}

	creds, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  out.AccessToken,
			"refreshToken": out.RefreshToken,
			"expiresAt":    time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UnixMilli(),
		},
	})
	fmt.Fprint(os.Stderr, "\nSuccess. Set this as the deploy secret:\n\n")
	fmt.Println(string(creds))
}

func randomURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		fatal("rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
