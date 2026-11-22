package syncer

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"whooper/internal/database"
)

// StatusOptions sets how fresh syncs must be for the history to count as complete.
type StatusOptions struct {
	// MaxSyncAge: every collection must have synced successfully within this long.
	MaxSyncAge time.Duration
	// MaxFullSyncAge: every collection must have had a full re-download within this long.
	MaxFullSyncAge time.Duration
}

func DefaultStatusOptions() StatusOptions {
	return StatusOptions{MaxSyncAge: 24 * time.Hour, MaxFullSyncAge: 8 * 24 * time.Hour}
}

type ResourceStatus struct {
	database.ResourceStats
	BackfillComplete bool       `json:"full_history_downloaded"`
	LastFullSyncAt   *time.Time `json:"last_full_sync_at,omitempty"`
	LastSyncAt       *time.Time `json:"last_sync_at,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
}

// Report answers "is my entire WHOOP history in the database?". Complete is true only when
// there are no Problems; Warnings are informational.
type Report struct {
	GeneratedAt   time.Time          `json:"generated_at"`
	Complete      bool               `json:"complete"`
	Problems      []string           `json:"problems"`
	Warnings      []string           `json:"warnings"`
	Token         database.TokenInfo `json:"token"`
	WhoopUserID   int64              `json:"whoop_user_id,omitempty"`
	Resources     []ResourceStatus   `json:"resources"`
	Integrity     database.Integrity `json:"integrity"`
	CycleGapCount int64              `json:"cycle_gap_count"`
	CycleGaps     []database.Gap     `json:"recent_cycle_gaps,omitempty"`
	LastRun       *database.SyncRun  `json:"last_run,omitempty"`
}

// cycleGapThreshold: consecutive cycles normally touch; a longer break is reported.
const cycleGapThreshold = time.Hour

func BuildReport(ctx context.Context, db *database.DB, opts StatusOptions) (*Report, error) {
	now := time.Now()
	r := &Report{GeneratedAt: now, Problems: []string{}, Warnings: []string{}}
	problem := func(format string, args ...any) { r.Problems = append(r.Problems, fmt.Sprintf(format, args...)) }
	warn := func(format string, args ...any) { r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...)) }

	var err error
	if r.Token, err = db.TokenInfo(ctx); err != nil {
		return nil, err
	}
	if !r.Token.Connected {
		problem("no WHOOP account connected: open /auth/login on the server")
	}
	if r.WhoopUserID, _, err = db.ProfileUserID(ctx); err != nil {
		return nil, err
	}

	for _, res := range database.Resources {
		stats, err := db.ResourceStats(ctx, res)
		if err != nil {
			return nil, err
		}
		st, err := db.SyncState(ctx, res)
		if err != nil {
			return nil, err
		}
		r.Resources = append(r.Resources, ResourceStatus{
			ResourceStats:    stats,
			BackfillComplete: st.BackfillComplete,
			LastFullSyncAt:   st.LastFullSyncAt,
			LastSyncAt:       st.LastSuccessAt,
			LastError:        st.LastError,
		})
		if st.LastError != "" {
			problem("%s: last sync failed (%s ago): %s", res, ago(now, st.LastErrorAt), st.LastError)
		}
		switch {
		case !st.BackfillComplete:
			problem("%s: full history has not been downloaded yet (no full sync has completed)", res)
		case now.Sub(*st.LastFullSyncAt) > opts.MaxFullSyncAge:
			problem("%s: last full re-download was %s ago (limit %s); is the `whooper sync --full` schedule running?",
				res, ago(now, st.LastFullSyncAt), human(opts.MaxFullSyncAge))
		}
		if st.LastSuccessAt != nil && now.Sub(*st.LastSuccessAt) > opts.MaxSyncAge {
			problem("%s: no successful sync for %s (limit %s); is the `whooper sync` schedule running?",
				res, ago(now, st.LastSuccessAt), human(opts.MaxSyncAge))
		}
	}

	if r.Integrity, err = db.Integrity(ctx); err != nil {
		return nil, err
	}
	in := r.Integrity
	if in.RecoveriesMissingSleep > 0 {
		problem("%d recoveries reference a sleep that isn't stored", in.RecoveriesMissingSleep)
	}
	if in.RecoveriesMissingCycle > 0 {
		problem("%d recoveries reference a cycle that isn't stored", in.RecoveriesMissingCycle)
	}
	if in.SleepsMissingCycle > 0 {
		problem("%d sleeps reference a cycle that isn't stored", in.SleepsMissingCycle)
	}
	if in.CompletedCyclesWithoutRecovery > 0 {
		warn("%d completed cycles have no recovery (normal for nights WHOOP couldn't score)", in.CompletedCyclesWithoutRecovery)
	}

	if r.CycleGapCount, r.CycleGaps, err = db.CycleGaps(ctx, cycleGapThreshold, 10); err != nil {
		return nil, err
	}
	if r.CycleGapCount > 0 {
		g := r.CycleGaps[0]
		warn("gaps longer than %s between consecutive cycles: %d, most recent %s → %s (%s); usually time the strap wasn't worn",
			human(cycleGapThreshold), r.CycleGapCount, g.From.UTC().Format("2006-01-02 15:04"), g.To.UTC().Format("2006-01-02 15:04"), human(g.To.Sub(g.From)))
	}

	if r.LastRun, err = db.LastRun(ctx); err != nil {
		return nil, err
	}
	r.Complete = len(r.Problems) == 0
	return r, nil
}

// WriteText renders the report for a terminal or a Dokploy schedule log.
func (r *Report) WriteText(w io.Writer) {
	verdict := "COMPLETE: the full WHOOP history is stored"
	if !r.Complete {
		verdict = "INCOMPLETE"
	}
	fmt.Fprintf(w, "WHOOP history: %s (checked %s)\n", verdict, r.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"))
	if r.WhoopUserID != 0 {
		fmt.Fprintf(w, "WHOOP user %d\n", r.WhoopUserID)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COLLECTION\tRECORDS\tDELETED\tEARLIEST\tLATEST\tFULL HISTORY\tLAST FULL SYNC\tLAST SYNC")
	for _, rs := range r.Resources {
		history := "no"
		if rs.BackfillComplete {
			history = "yes"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			rs.Resource, rs.Live, rs.Deleted, date(rs.Earliest), date(rs.Latest), history,
			stamp(rs.LastFullSyncAt), stamp(rs.LastSyncAt))
	}
	tw.Flush()
	if r.LastRun != nil {
		fmt.Fprintf(w, "\nLast run: %s, mode %s, started %s, %d API requests\n",
			r.LastRun.Status, r.LastRun.Mode, r.LastRun.StartedAt.UTC().Format("2006-01-02 15:04 UTC"), r.LastRun.Requests)
	}
	list(w, "Problems", r.Problems)
	list(w, "Warnings", r.Warnings)
}

func list(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, it := range items {
		fmt.Fprintf(w, "  - %s\n", it)
	}
}

func date(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}

func stamp(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func ago(now time.Time, t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return human(now.Sub(*t))
}

func human(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	s := d.Round(time.Minute).String() // e.g. "1h0m0s", "10m0s"
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}
