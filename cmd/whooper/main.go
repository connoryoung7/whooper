// Command whooper mirrors a WHOOP account's complete history into Postgres.
//
//	whooper serve          web server: WHOOP OAuth connect flow, /status, /health
//	whooper sync [--full]  one sync pass, then exit (run this on a schedule)
//	whooper status         completeness report; exits 1 if the history is incomplete
//	whooper migrate [status|up|down]  inspect or change the schema version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"

	"whooper/internal/config"
	"whooper/internal/database"
	"whooper/internal/server"
	"whooper/internal/syncer"
	"whooper/internal/whoop"
)

var errIncomplete = errors.New("WHOOP history is incomplete")

func main() {
	_ = godotenv.Load() // local development convenience; real deployments use env vars

	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, args)
	case "sync":
		err = runSync(ctx, args)
	case "status":
		err = status(ctx, args)
	case "migrate":
		err = migrate(ctx, args)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		if !errors.Is(err, errIncomplete) {
			log.Printf("whooper %s: %v", cmd, err)
		}
		stop()
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: whooper <command> [flags]

commands:
  serve          run the web server (WHOOP connect flow at /auth/login, report at /status)
  sync [--full]  sync once and exit; --full re-downloads the entire history
  status         print the completeness report; exit status 1 if incomplete
  migrate [status|up|down]
                 show applied migrations (default), apply pending ones, or roll back
                 the newest one (every command already applies pending migrations)
`)
}

type app struct {
	cfg    config.Config
	db     *database.DB
	oauth  *whoop.OAuth
	api    *whoop.Client
	syncer *syncer.Syncer
}

func setup(ctx context.Context, required ...string) (*app, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := cfg.Require(required...); err != nil {
		return nil, err
	}
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	oauth := whoop.NewOAuth(cfg.ClientID, cfg.ClientSecret, cfg.RedirectURL)
	api := whoop.NewClient(&whoop.TokenSource{OAuth: oauth, Store: db})
	return &app{
		cfg:    cfg,
		db:     db,
		oauth:  oauth,
		api:    api,
		syncer: &syncer.Syncer{DB: db, API: api, Lookback: cfg.SyncLookback, Logf: log.Printf},
	}, nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := setup(ctx, "WHOOP_CLIENT_ID", "WHOOP_CLIENT_SECRET", "WHOOP_REDIRECT_URL", "ADMIN_PASSWORD")
	if err != nil {
		return err
	}
	defer a.db.Close()

	srv := server.New(server.Options{
		DB:            a.db,
		OAuth:         a.oauth,
		API:           a.api,
		Syncer:        a.syncer,
		AdminPassword: a.cfg.AdminPassword,
		BaseContext:   ctx,
		Logf:          log.Printf,
	})
	httpSrv := srv.HTTPServer(a.cfg.Port)
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	log.Printf("listening on :%d", a.cfg.Port)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = httpSrv.Shutdown(shutdownCtx)
	srv.Wait() // background syncs stop because ctx is cancelled
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func runSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	full := fs.Bool("full", false, "re-download the entire history and detect records deleted in WHOOP\n"+
		"(without it, each collection is synced incrementally once its history is downloaded)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := setup(ctx, "WHOOP_CLIENT_ID", "WHOOP_CLIENT_SECRET")
	if err != nil {
		return err
	}
	defer a.db.Close()

	err = a.syncer.Run(ctx, *full)
	if errors.Is(err, syncer.ErrLocked) {
		log.Printf("%v; skipping this run", err)
		return nil
	}
	// Print the report either way so every scheduled run's log shows where things stand.
	if rep, repErr := syncer.BuildReport(ctx, a.db, syncer.DefaultStatusOptions()); repErr == nil {
		fmt.Println()
		rep.WriteText(os.Stdout)
	}
	return err
}

func status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	def := syncer.DefaultStatusOptions()
	maxSync := fs.Duration("max-sync-age", def.MaxSyncAge, "incomplete if any collection hasn't synced for this long")
	maxFull := fs.Duration("max-full-sync-age", def.MaxFullSyncAge, "incomplete if any collection hasn't had a full sync for this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := setup(ctx)
	if err != nil {
		return err
	}
	defer a.db.Close()

	rep, err := syncer.BuildReport(ctx, a.db, syncer.StatusOptions{MaxSyncAge: *maxSync, MaxFullSyncAge: *maxFull})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		rep.WriteText(os.Stdout)
	}
	if !rep.Complete {
		return errIncomplete
	}
	return nil
}

func migrate(ctx context.Context, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	switch sub {
	case "status":
		migrations, err := db.MigrationStatus(ctx)
		if err != nil {
			return err
		}
		for _, m := range migrations {
			state := "pending"
			if m.State == goose.StateApplied {
				// goose stores this as a timestamp without time zone, in the database
				// server's local time, so don't claim a zone for it.
				state = "applied " + m.AppliedAt.Format("2006-01-02 15:04") + " (database server time)"
			}
			fmt.Printf("%-45s %s\n", m.Source.Path, state)
		}
	case "up":
		results, err := db.MigrateUp(ctx)
		for _, r := range results {
			fmt.Printf("applied %s (%s)\n", r.Source.Path, r.Duration.Round(time.Millisecond))
		}
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Println("no pending migrations")
		}
	case "down":
		v, err := db.MigrationVersion(ctx)
		if err != nil {
			return err
		}
		if v <= 1 {
			return errors.New("refusing to roll back the initial schema: it would drop every table, including the WHOOP token")
		}
		r, err := db.MigrateDown(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("rolled back %s\n", r.Source.Path)
	default:
		return fmt.Errorf("unknown migrate command %q (want status, up or down)", sub)
	}
	return nil
}
