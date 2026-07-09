package main

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	appEnv := loadEnvFiles()
	setupLogger(appEnv)
	slog.Info("starting", "env", appEnv)

	if os.Getenv("AUTH_TOKEN") == "" {
		slog.Warn("AUTH_TOKEN not set, /api/usage is open to anyone who can reach this server")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/usage", requireAuth(handleUsage))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	handler := recoverPanic(requestLogger(mux))
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	slog.Info("listening", "port", port)
	if err := server.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// requireAuth gates a handler behind AUTH_TOKEN when it is set. The Playdate
// HTTP API can send headers, but a query parameter is also accepted because
// it is easier to bake into a single URL on the device.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := os.Getenv("AUTH_TOKEN")
		if want != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.URL.Query().Get("token")
			}
			if got != want {
				writeJSONError(w, http.StatusUnauthorized, "missing or invalid token")
				return
			}
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// loadEnvFiles loads env files for local development. APP_ENV picks the
// profile: "dev" (default) loads .env.dev, "prod" loads .env.prod, then
// .env as the shared fallback. Already-set variables always win, so
// deployment secrets are never overridden and missing files are fine.
func loadEnvFiles() string {
	appEnv := os.Getenv("APP_ENV")
	if appEnv == "" {
		appEnv = "dev"
	}
	loadDotEnv(filepath.Join(".", ".env."+appEnv))
	loadDotEnv(filepath.Join(".", ".env"))
	return appEnv
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}
