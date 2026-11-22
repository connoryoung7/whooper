// Package server is the small web front end: WHOOP OAuth connect flow, a completeness
// report, and a health check for Dokploy.
package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"whooper/internal/database"
	"whooper/internal/syncer"
	"whooper/internal/whoop"
)

type Options struct {
	DB     *database.DB
	OAuth  *whoop.OAuth
	API    *whoop.Client
	Syncer *syncer.Syncer
	// AdminPassword protects /auth/login and /status with HTTP Basic auth (any username).
	AdminPassword string
	// BaseContext bounds background syncs started by the server; cancel it on shutdown.
	BaseContext   context.Context
	StatusOptions syncer.StatusOptions
	Logf          func(format string, args ...any)
}

type Server struct {
	opts Options
	wg   sync.WaitGroup
}

func New(opts Options) *Server {
	if opts.BaseContext == nil {
		opts.BaseContext = context.Background()
	}
	if opts.StatusOptions == (syncer.StatusOptions{}) {
		opts.StatusOptions = syncer.DefaultStatusOptions()
	}
	return &Server{opts: opts}
}

// HTTPServer wraps the handler in an http.Server listening on port.
func (s *Server) HTTPServer(port int) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       time.Minute,
	}
}

// Wait blocks until background syncs started by the server have returned.
func (s *Server) Wait() { s.wg.Wait() }

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}
