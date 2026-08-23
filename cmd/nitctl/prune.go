package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os/user"
	"time"

	"github.com/NitScm/nit/internal/bootstrap"
	"github.com/NitScm/nit/internal/store/connect"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// auditPrune removes audit records older than a cutoff.
//
// It talks to the database rather than the API, unlike every other `nitctl
// audit` subcommand. That is deliberate: the server holds a store.Store and
// cannot reach a pruner through it, so there is no endpoint to call and no way
// for a request to trigger a purge. Retention is something an operator does at
// a shell, with the database credentials, having typed a date.
//
// It refuses to delete anything until told twice. The first run counts and
// reports; -yes performs it. An audit trail is the one table where "I meant the
// other date" has no remedy.
func auditPrune(args []string) error {
	fs := flag.NewFlagSet("audit prune", flag.ContinueOnError)

	before := fs.String("before", "", "remove records older than this date (YYYY-MM-DD or RFC3339)")
	keepDays := fs.Int("keep-days", 0, "remove records older than this many days")
	batch := fs.Int("batch", 1000, "rows per batch; smaller holds locks for less time")
	yes := fs.Bool("yes", false, "actually delete; without it the command only reports")
	dsn := fs.String("dsn", "", "database DSN (defaults to the configured database.url)")
	configFile := fs.String("config", "", "configuration file to read the DSN from")
	actor := fs.String("actor", "", "who is running this, recorded in the trail (defaults to the OS user)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cutoff, err := resolveCutoff(*before, *keepDays)
	if err != nil {
		return err
	}

	resolved := *dsn
	if resolved == "" {
		cfg, err := bootstrap.LoadConfigFrom(*configFile)
		if err == nil {
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

	pruner, ok := st.(store.AuditPruner)
	if !ok {
		return fmt.Errorf("this backend cannot prune the audit trail")
	}

	matched, err := pruner.CountAuditBefore(ctx, cutoff)
	if err != nil {
		return err
	}

	fmt.Printf("cutoff:  %s\n", cutoff.Format(time.RFC3339))
	fmt.Printf("matched: %d record(s) older than that\n", matched)

	if !*yes {
		fmt.Println()
		fmt.Println("nothing was deleted. re-run with -yes to remove them.")
		fmt.Println("there is no undo: the audit trail is the record that nit enforced anything.")

		return nil
	}

	if matched == 0 {
		fmt.Println("nothing to do")
		return nil
	}

	who := *actor
	if who == "" {
		who = osUser()
	}

	// Recorded before the deletion, and again after. An interrupted purge then
	// leaves a "started" with no "completed", which is exactly the trace an
	// auditor needs to see — the alternative is a gap in the trail that nothing
	// explains.
	started := time.Now().UTC()

	if err := appendPurgeRecord(ctx, st, "audit.purge_started", who, started, map[string]string{
		"cutoff":  cutoff.Format(time.RFC3339),
		"matched": fmt.Sprintf("%d", matched),
	}); err != nil {
		return fmt.Errorf("record the purge before running it: %w", err)
	}

	result, err := pruner.PruneAudit(ctx, cutoff, *batch)

	if result.GuardsWereMissing {
		fmt.Println()
		fmt.Println("WARNING: the append-only protection was already absent when this started.")
		fmt.Println("a previous purge did not finish, and audit_log has accepted deletions since.")
		fmt.Println("records removed in that window left no trace. it has been restored now.")
	}

	if err != nil {
		return fmt.Errorf("purge after removing %d record(s): %w", result.Removed, err)
	}

	if recordErr := appendPurgeRecord(ctx, st, "audit.purge_completed", who, time.Now().UTC(), map[string]string{
		"cutoff":  cutoff.Format(time.RFC3339),
		"removed": fmt.Sprintf("%d", result.Removed),
	}); recordErr != nil {
		// The rows are gone either way; failing here would suggest otherwise.
		fmt.Printf("removed %d record(s), but the completion could not be recorded: %v\n",
			result.Removed, recordErr)

		return nil
	}

	fmt.Printf("removed %d record(s)\n", result.Removed)

	return nil
}

// resolveCutoff turns the two ways of naming a date into one instant.
func resolveCutoff(before string, keepDays int) (time.Time, error) {
	switch {
	case before != "" && keepDays > 0:
		return time.Time{}, fmt.Errorf("give -before or -keep-days, not both")
	case before == "" && keepDays <= 0:
		return time.Time{}, fmt.Errorf("give -before <date> or -keep-days <n>")
	}

	if keepDays > 0 {
		return time.Now().UTC().AddDate(0, 0, -keepDays), nil
	}

	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, before); err == nil {
			cutoff := parsed.UTC()

			// A cutoff in the future would delete the whole table, and it is
			// the kind of typo — a year off — that a tool should refuse rather
			// than execute faithfully.
			if cutoff.After(time.Now().UTC()) {
				return time.Time{}, fmt.Errorf(
					"the cutoff %s is in the future; that would remove every record",
					cutoff.Format(time.RFC3339))
			}

			return cutoff, nil
		}
	}

	return time.Time{}, fmt.Errorf("cannot read %q as a date; use YYYY-MM-DD or RFC3339", before)
}

// osUser names whoever ran the command, for the record the purge leaves.
//
// Best effort: an unnamed operator is worse than an approximate one, and this
// is the same identity the shell already shows.
func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}

	return "unknown"
}

func appendPurgeRecord(ctx context.Context, st store.Store, action, actor string, at time.Time, detail map[string]string) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}

	return st.Audit().Append(ctx, &store.AuditRecord{
		TenantID:      policy.DefaultTenant,
		OccurredAt:    at,
		ActorLabel:    actor,
		Action:        action,
		PolicyVersion: "n/a",
		Detail:        encoded,
	})
}
