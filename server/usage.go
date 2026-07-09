package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GET /api/usage — the one call the Playdate makes. Everything upstream
// (token lookup, the OAuth-only usage endpoint, response shaping) is hidden
// behind it. Responses are cached briefly so the device can poll freely
// without hammering Anthropic.

const (
	usageURL      = "https://api.anthropic.com/api/oauth/usage"
	usageCacheTTL = 60 * time.Second
	// Serve a stale cached payload for a while when upstream fails, so a
	// blip never blanks the Playdate screen.
	usageStaleMax = 15 * time.Minute
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// upstreamUsage is the subset of the oauth/usage response we care about.
// The `limits` array is the same data the claude.ai usage screen renders.
type upstreamUsage struct {
	Limits []struct {
		Kind     string    `json:"kind"`  // session | weekly_all | weekly_scoped
		Group    string    `json:"group"` // session | weekly
		Percent  float64   `json:"percent"`
		Severity string    `json:"severity"`
		ResetsAt time.Time `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

// usagePayload is what the Playdate receives. resets_in is pre-formatted
// ("4h 32m") so the device does no date math.
type usagePayload struct {
	Session   *usageLimit  `json:"session"`
	Weekly    *usageLimit  `json:"weekly"`
	Models    []usageLimit `json:"models"`
	FetchedAt time.Time    `json:"fetched_at"`
	Stale     bool         `json:"stale,omitempty"`
}

type usageLimit struct {
	Name           string    `json:"name,omitempty"`
	Percent        int       `json:"percent"`
	Severity       string    `json:"severity"`
	ResetsAt       time.Time `json:"resets_at"`
	ResetsInSecs   int64     `json:"resets_in_secs"`
	ResetsInPretty string    `json:"resets_in"`
}

var usageMu sync.Mutex
var cachedUsage *usagePayload

func handleUsage(w http.ResponseWriter, r *http.Request) {
	// format=plain emits pipe-separated lines instead of JSON, so the
	// Playdate C client can parse with strtok instead of a JSON decoder.
	plain := r.URL.Query().Get("format") == "plain"
	respond := func(p usagePayload) {
		if plain {
			writePlain(w, p)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}

	usageMu.Lock()
	defer usageMu.Unlock()

	if cachedUsage != nil && time.Since(cachedUsage.FetchedAt) < usageCacheTTL {
		respond(refreshCountdowns(*cachedUsage))
		return
	}

	payload, err := fetchUsage()
	if err != nil {
		if cachedUsage != nil && time.Since(cachedUsage.FetchedAt) < usageStaleMax {
			slog.Warn("serving stale usage", "err", err)
			stale := refreshCountdowns(*cachedUsage)
			stale.Stale = true
			respond(stale)
			return
		}
		slog.Error("usage fetch failed", "err", err)
		if plain {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "err|"+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	cachedUsage = payload
	respond(*payload)
}

// writePlain renders the payload as one line per bar:
//
//	ok            (or "stale")
//	session|63|1h 57m
//	weekly|59|3d 18h
//	Fable|6|3d 18h
func writePlain(w http.ResponseWriter, p usagePayload) {
	var b strings.Builder
	if p.Stale {
		b.WriteString("stale\n")
	} else {
		b.WriteString("ok\n")
	}
	if p.Session != nil {
		fmt.Fprintf(&b, "session|%d|%s\n", p.Session.Percent, p.Session.ResetsInPretty)
	}
	if p.Weekly != nil {
		fmt.Fprintf(&b, "weekly|%d|%s\n", p.Weekly.Percent, p.Weekly.ResetsInPretty)
	}
	for _, m := range p.Models {
		fmt.Fprintf(&b, "%s|%d|%s\n", m.Name, m.Percent, m.ResetsInPretty)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(b.String()))
}

func fetchUsage() (*usagePayload, error) {
	token, err := accessToken()
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest(http.MethodGet, usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		// Token revoked or rotated out from under us; drop the cache so the
		// next request re-reads the Keychain.
		invalidateToken()
		return nil, fmt.Errorf("upstream rejected token (401); will re-read credentials on next request")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var upstream upstreamUsage
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, fmt.Errorf("parse usage response: %w", err)
	}

	now := time.Now()
	payload := &usagePayload{FetchedAt: now, Models: []usageLimit{}}
	for _, l := range upstream.Limits {
		limit := usageLimit{
			Percent:  int(l.Percent),
			Severity: l.Severity,
			ResetsAt: l.ResetsAt,
		}
		switch l.Kind {
		case "session":
			payload.Session = &limit
		case "weekly_all":
			payload.Weekly = &limit
		case "weekly_scoped":
			if l.Scope != nil && l.Scope.Model != nil {
				limit.Name = l.Scope.Model.DisplayName
			}
			payload.Models = append(payload.Models, limit)
		}
	}
	*payload = refreshCountdowns(*payload)
	return payload, nil
}

// refreshCountdowns recomputes the relative reset fields from resets_at so
// cached responses still show an accurate countdown.
func refreshCountdowns(p usagePayload) usagePayload {
	now := time.Now()
	fill := func(l *usageLimit) {
		d := l.ResetsAt.Sub(now)
		if d < 0 {
			d = 0
		}
		l.ResetsInSecs = int64(d.Seconds())
		l.ResetsInPretty = prettyDuration(d)
	}
	if p.Session != nil {
		s := *p.Session
		fill(&s)
		p.Session = &s
	}
	if p.Weekly != nil {
		wk := *p.Weekly
		fill(&wk)
		p.Weekly = &wk
	}
	models := make([]usageLimit, len(p.Models))
	for i, m := range p.Models {
		fill(&m)
		models[i] = m
	}
	p.Models = models
	return p
}

// prettyDuration renders like the claude.ai usage screen: "4h 32m",
// "3d 14h", "12m".
func prettyDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
