package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/NitScm/nit/internal/bootstrap"
	"github.com/NitScm/nit/internal/store/connect"
	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// auditExport writes decision records to stdout as JSON Lines.
//
// It exists because of what a Sink is not: never the only copy. Every record is
// in the database first, and an export that was unreachable for an hour has a
// gap the database can fill — but only if there is a way to get the records out
// of it. A bigger buffer is not that way, and pretending otherwise is how a
// deployment discovers the gap during an audit.
//
// Why stdout rather than a sink. A sink is compiled into a deployment's own
// binary through nitd.Deps.AuditSink; nitctl has none and cannot have one
// without inventing a plugin mechanism. JSON Lines feeds anything — a curl into
// a SIEM, a file on an object store, jq — and costs nothing to consume.
//
// Why an operator command rather than something that follows the writer. Ids
// are handed out at insert and become visible at commit, so a reader that
// treats "the highest id right now" as "everything so far" can miss a
// transaction that started earlier and committed later. Over a window that has
// already settled — which is what an operator filling a gap asks for — there is
// no such row. The cursor is exact within the window and unsound past its end,
// and this command only offers the sound half.
func auditExport(args []string) error {
	fs := flag.NewFlagSet("audit export", flag.ContinueOnError)

	since := fs.String("since", "", "start of the window (YYYY-MM-DD, RFC3339, or a duration like 24h)")
	until := fs.String("until", "", "end of the window (defaults to now minus -settle)")
	settle := fs.Duration("settle", time.Minute, "how far back the window must end, so no transaction is still in flight")
	afterID := fs.Int64("after-id", 0, "resume after this cursor, as printed by a previous run")
	tenant := fs.String("tenant", string(policy.DefaultTenant), "tenant whose trail to export")
	batch := fs.Int("batch", 1000, "rows per query")
	dsn := fs.String("dsn", "", "database DSN (defaults to the configured database.url)")
	configFile := fs.String("config", "", "configuration file to read the DSN from")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *since == "" && *afterID == 0 {
		return fmt.Errorf("-since or -after-id is required: exporting the whole trail by " +
			"accident is not something this should make easy")
	}

	window, err := resolveWindow(*since, *until, *settle)
	if err != nil {
		return err
	}

	resolved := *dsn
	if resolved == "" {
		if cfg, err := bootstrap.LoadConfigFrom(*configFile); err == nil {
			resolved = cfg.DatabaseURL
		}
	}
	if resolved == "" {
		return fmt.Errorf("-dsn is required (or set database.url, or %s)", bootstrap.EnvDatabaseURL)
	}

	ctx := context.Background()

	st, err := connect.Open(ctx, resolved)
	if err != nil {
		return err
	}
	defer st.Close()

	// Repository identities are what a record carries to a destination that has
	// never heard of this database, so the row ids have to be resolved to them
	// before anything is written.
	repositories, err := repositoryNames(ctx, st, policy.TenantID(*tenant))
	if err != nil {
		return err
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	encoder := json.NewEncoder(out)

	var (
		cursor  = *afterID
		written int
	)

	for {
		page, err := st.Audit().Query(ctx, store.AuditQuery{
			Tenant:  policy.TenantID(*tenant),
			Since:   window.since,
			Until:   window.until,
			AfterID: cursor,
			Oldest:  true,
			Limit:   *batch,
		})
		if err != nil {
			return err
		}

		if len(page) == 0 {
			break
		}

		for _, record := range page {
			if err := encoder.Encode(exportRecord(record, repositories)); err != nil {
				return err
			}

			cursor = record.ID
			written++
		}

		// Flushing per page rather than at the end, so a long export is a
		// stream a consumer can work through rather than a wait.
		if err := out.Flush(); err != nil {
			return err
		}
	}

	if err := out.Flush(); err != nil {
		return err
	}

	// stdout is the data; this is the operator's. Printing the cursor to stdout
	// would put a database row id in the export, which is the one thing a
	// record deliberately does not carry.
	fmt.Fprintf(os.Stderr, "exported %d records up to %s; resume with -after-id %d\n",
		written, window.until.Format(time.RFC3339), cursor)

	return nil
}

type exportWindow struct {
	since time.Time
	until time.Time
}

// resolveWindow reads the two bounds and refuses one that has not settled.
func resolveWindow(since, until string, settle time.Duration) (exportWindow, error) {
	var w exportWindow

	ceiling := time.Now().UTC().Add(-settle)

	if since != "" {
		parsed, err := parseWhen(since)
		if err != nil {
			return w, fmt.Errorf("-since: %w", err)
		}

		w.since = parsed
	}

	if until == "" {
		w.until = ceiling
	} else {
		parsed, err := parseWhen(until)
		if err != nil {
			return w, fmt.Errorf("-until: %w", err)
		}

		w.until = parsed
	}

	// A window whose end is now includes records from transactions that have
	// not committed yet, and the cursor would step past them. Refusing beats
	// exporting a gap that looks complete.
	if w.until.After(ceiling) {
		return w, fmt.Errorf("-until is less than %s in the past; a transaction may still "+
			"be in flight and the export would skip it", settle)
	}

	if !w.since.IsZero() && !w.until.After(w.since) {
		return w, fmt.Errorf("the window ends before it starts")
	}

	return w, nil
}

// parseWhen reads a date, an RFC3339 timestamp, or a duration back from now.
func parseWhen(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}

	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t.UTC(), nil
	}

	if d, err := time.ParseDuration(value); err == nil {
		if d < 0 {
			d = -d
		}

		return time.Now().UTC().Add(-d), nil
	}

	return time.Time{}, fmt.Errorf("%q is not a date, an RFC3339 timestamp, or a duration", value)
}

// repositoryNames maps row ids onto the identities a bundle uses.
func repositoryNames(ctx context.Context, st store.Store, tenant policy.TenantID) (map[store.ID]string, error) {
	repos, err := st.Repositories().List(ctx, tenant)
	if err != nil {
		return nil, err
	}

	out := make(map[store.ID]string, len(repos))
	for _, repo := range repos {
		out[repo.ID] = string(repo.PolicyRepoID)
	}

	return out, nil
}

// exportRecord projects a stored record onto the shape a sink receives, which
// is the shape a destination has already been taught to read.
func exportRecord(r *store.AuditRecord, repositories map[store.ID]string) audit.Record {
	return audit.Record{
		OccurredAt:    r.OccurredAt,
		Actor:         r.ActorLabel,
		Action:        r.Action,
		Repository:    repositories[r.RepositoryID],
		Branch:        r.Branch,
		Path:          r.Path,
		Effect:        string(r.Effect),
		Reason:        string(r.Reason),
		RuleID:        r.RuleID,
		Guard:         r.Guard,
		PolicyVersion: r.PolicyVersion,
		RequestID:     r.RequestID,
		TaskID:        string(r.TaskID),
	}
}
