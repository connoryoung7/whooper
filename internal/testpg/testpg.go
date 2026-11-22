// Package testpg runs a throwaway Postgres container for integration tests (requires Docker).
package testpg

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

type Server struct {
	container *postgres.PostgresContainer
	admin     *sql.DB
	base      *url.URL
	n         atomic.Int64
}

func Start(ctx context.Context) (*Server, error) {
	c, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("whooper"),
		postgres.WithUsername("whooper"),
		postgres.WithPassword("whooper"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		return nil, err
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		c.Terminate(ctx)
		return nil, err
	}
	base, err := url.Parse(dsn)
	if err != nil {
		c.Terminate(ctx)
		return nil, err
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		c.Terminate(ctx)
		return nil, err
	}
	return &Server{container: c, admin: admin, base: base}, nil
}

// NewDatabase creates an empty database for one test and returns its connection string.
func (s *Server) NewDatabase(t testing.TB) string {
	t.Helper()
	name := fmt.Sprintf("test_%d", s.n.Add(1))
	if _, err := s.admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("creating database: %v", err)
	}
	u := *s.base
	u.Path = "/" + name
	return u.String()
}

func (s *Server) Close() {
	s.admin.Close()
	s.container.Terminate(context.Background())
}
