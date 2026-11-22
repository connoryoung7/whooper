// Package config reads settings from the environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port          int
	DatabaseURL   string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	AdminPassword string
	SyncLookback  time.Duration
}

// Load reads the configuration. DATABASE_URL wins; otherwise the BLUEPRINT_DB_* variables
// used by docker-compose.yml for local development are combined into one.
func Load() (Config, error) {
	c := Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		ClientID:      os.Getenv("WHOOP_CLIENT_ID"),
		ClientSecret:  os.Getenv("WHOOP_CLIENT_SECRET"),
		RedirectURL:   os.Getenv("WHOOP_REDIRECT_URL"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
	}
	var err error
	if c.Port, err = intEnv("PORT", 8080); err != nil {
		return c, err
	}
	days, err := intEnv("SYNC_LOOKBACK_DAYS", 14)
	if err != nil {
		return c, err
	}
	if days < 1 {
		return c, errors.New("SYNC_LOOKBACK_DAYS must be at least 1")
	}
	c.SyncLookback = time.Duration(days) * 24 * time.Hour
	if c.DatabaseURL == "" {
		c.DatabaseURL = blueprintDSN()
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is not set")
	}
	return c, nil
}

// Require fails if any of the named environment variables is empty.
func (c Config) Require(names ...string) error {
	values := map[string]string{
		"WHOOP_CLIENT_ID":     c.ClientID,
		"WHOOP_CLIENT_SECRET": c.ClientSecret,
		"WHOOP_REDIRECT_URL":  c.RedirectURL,
		"ADMIN_PASSWORD":      c.AdminPassword,
	}
	var missing []string
	for _, n := range names {
		v, known := values[n]
		if !known {
			panic("config: unknown variable " + n)
		}
		if v == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

func blueprintDSN() string {
	host := os.Getenv("BLUEPRINT_DB_HOST")
	if host == "" {
		return ""
	}
	port := os.Getenv("BLUEPRINT_DB_PORT")
	if port == "" {
		port = "5432"
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(os.Getenv("BLUEPRINT_DB_USERNAME"), os.Getenv("BLUEPRINT_DB_PASSWORD")),
		Host:   host + ":" + port,
		Path:   "/" + os.Getenv("BLUEPRINT_DB_DATABASE"),
	}
	q := url.Values{"sslmode": {"disable"}}
	if s := os.Getenv("BLUEPRINT_DB_SCHEMA"); s != "" {
		q.Set("search_path", s)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func intEnv(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, v)
	}
	return n, nil
}
