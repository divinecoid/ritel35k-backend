// Package config loads runtime configuration from environment (optionally seeded from a .env file).
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Env            string
	HTTPAddr       string
	DatabaseURL    string
	RedisURL       string
	JWTSecret      string
	AccessTTL      time.Duration
	RefreshTTL     time.Duration
	CORSOrigins    []string
	LogLevel       string
	WorkerPoll     time.Duration
	TrustProxy     bool
	AuthRatePerMin int
	AuthRateBurst  int
}

func Load() (Config, error) {
	loadDotEnv(".env")
	c := Config{
		Env:         get("APP_ENV", "dev"),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		DatabaseURL: get("DATABASE_URL", ""),
		RedisURL:    get("REDIS_URL", ""),
		JWTSecret:   get("JWT_SECRET", ""),
		LogLevel:    get("LOG_LEVEL", "info"),
		TrustProxy:  get("TRUST_PROXY", "false") == "true",
	}
	c.AuthRatePerMin = atoi(get("AUTH_RATE_PER_MIN", "60"))
	c.AuthRateBurst = atoi(get("AUTH_RATE_BURST", "20"))
	var err error
	if c.AccessTTL, err = time.ParseDuration(get("ACCESS_TTL", "15m")); err != nil {
		return c, fmt.Errorf("ACCESS_TTL: %w", err)
	}
	if c.RefreshTTL, err = time.ParseDuration(get("REFRESH_TTL", "720h")); err != nil {
		return c, fmt.Errorf("REFRESH_TTL: %w", err)
	}
	if c.WorkerPoll, err = time.ParseDuration(get("WORKER_POLL", "1s")); err != nil {
		return c, fmt.Errorf("WORKER_POLL: %w", err)
	}
	for _, o := range strings.Split(get("CORS_ORIGINS", "http://localhost:5173"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return c, errors.New("JWT_SECRET must be at least 32 characters")
	}
	return c, nil
}

func get(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

// loadDotEnv sets variables from a KEY=VALUE file without overriding the real environment.
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
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
