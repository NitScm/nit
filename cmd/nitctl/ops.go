package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/NitScm/nit/internal/server"
	"github.com/NitScm/nit/internal/workspace"
)

// The operations commands read the API rather than the database.
//
// That is the architectural rule the console depends on: nitctl and the web UI
// are both clients of the same endpoints, so the API is exercised from day one
// and the UI can never need a capability nitctl lacks. Only the commands an
// operator runs on the server itself — migrate, token create — touch the
// database directly.

// opsClient calls the operations API.
type opsClient struct {
	baseURL string
	token   string
}

// newOpsClient resolves the server and token from flags, the environment, or
// the stored credentials.
func newOpsClient(fs *flag.FlagSet, server, token *string) (*opsClient, error) {
	resolved := *server
	if resolved == "" {
		resolved = os.Getenv("NIT_SERVER")
	}

	value := *token
	if value == "" {
		value = os.Getenv("NIT_TOKEN")
	}

	if resolved != "" && value == "" {
		// Reuse the credential "nit login" stored, so an operator with a
		// developer install does not have to paste a token twice.
		creds, err := workspace.LoadCredentials()
		if err == nil {
			value, _ = creds.Token(resolved)
		}
	}

	if resolved == "" || value == "" {
		return nil, fmt.Errorf("-server and -token are required (or set NIT_SERVER and NIT_TOKEN)")
	}

	return &opsClient{baseURL: strings.TrimRight(resolved, "/"), token: value}, nil
}

func (c *opsClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("not found; is this account in one of the server's NIT_ADMIN_GROUPS?")
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// ops dispatches the operations subcommands.
func ops(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)

	serverURL := fs.String("server", "", "nit server URL (defaults to $NIT_SERVER)")
	token := fs.String("token", "", "operator token (defaults to $NIT_TOKEN or the stored credential)")

	state := fs.String("state", "", "filter tasks by state")
	kind := fs.String("kind", "", "filter tasks by kind")
	repository := fs.String("repository", "", "filter by repository")
	user := fs.String("user", "", "filter audit records by user")
	requestID := fs.String("request", "", "filter audit records by request id")
	since := fs.Duration("since", 0, "only audit records from the last duration")
	limit := fs.Int("limit", 50, "maximum rows")
	asJSON := fs.Bool("json", false, "emit raw JSON")

	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := newOpsClient(fs, serverURL, token)
	if err != nil {
		return err
	}

	ctx := context.Background()

	switch command {
	case "stats":
		return client.stats(ctx, *asJSON)
	case "tasks":
		return client.tasks(ctx, *state, *kind, *repository, *limit, *asJSON)
	case "audit":
		return client.audit(ctx, *user, *repository, *requestID, *since, *limit, *asJSON)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func (c *opsClient) stats(ctx context.Context, asJSON bool) error {
	var stats server.Stats
	if err := c.get(ctx, "/v1/admin/stats", &stats); err != nil {
		return err
	}

	if asJSON {
		return emitJSON(stats)
	}

	fmt.Printf("policy:       %s\n", stats.PolicyVersion)
	fmt.Printf("repositories: %d\n", stats.Repositories)
	fmt.Println()
	fmt.Printf("queued:       %d\n", stats.Tasks["queued"])
	fmt.Printf("running:      %d\n", stats.Tasks["running"])
	fmt.Printf("succeeded:    %d\n", stats.Tasks["succeeded"])
	fmt.Printf("failed:       %d\n", stats.Tasks["failed"])
	fmt.Println()
	fmt.Printf("busy branches:   %d\n", stats.BusyBranches)
	fmt.Printf("denials (24h):   %d\n", stats.RecentDenials)

	return nil
}

func (c *opsClient) tasks(ctx context.Context, state, kind, repository string, limit int, asJSON bool) error {
	query := fmt.Sprintf("?limit=%d", limit)

	for _, param := range []struct{ name, value string }{
		{"state", state}, {"kind", kind}, {"repository", repository},
	} {
		if param.value != "" {
			query += "&" + param.name + "=" + param.value
		}
	}

	var tasks []server.TaskView
	if err := c.get(ctx, "/v1/admin/tasks"+query, &tasks); err != nil {
		return err
	}

	if asJSON {
		return emitJSON(tasks)
	}

	if len(tasks) == 0 {
		fmt.Println("no tasks")
		return nil
	}

	fmt.Printf("%-38s %-6s %-10s %-12s %-24s %-10s %s\n",
		"TASK", "KIND", "STATE", "USER", "REPOSITORY@BRANCH", "DURATION", "NOTE")

	for _, task := range tasks {
		note := ""

		switch {
		case task.Error != nil:
			note = task.Error.Code
		case task.QueuePosition > 0:
			note = fmt.Sprintf("%d ahead", task.QueuePosition)
		case task.LeaseHolder != "":
			note = task.LeaseHolder
		}

		fmt.Printf("%-38s %-6s %-10s %-12s %-24s %-10s %s\n",
			task.ID, task.Kind, task.State, task.User,
			task.Repository+"@"+task.Branch,
			formatDuration(task.DurationMs), note)
	}

	return nil
}

func (c *opsClient) audit(ctx context.Context, user, repository, requestID string, since time.Duration, limit int, asJSON bool) error {
	query := fmt.Sprintf("?limit=%d", limit)

	for _, param := range []struct{ name, value string }{
		{"user", user}, {"repository", repository}, {"request_id", requestID},
	} {
		if param.value != "" {
			query += "&" + param.name + "=" + param.value
		}
	}

	if since > 0 {
		query += "&since=" + time.Now().UTC().Add(-since).Format(time.RFC3339)
	}

	var records []server.AuditView
	if err := c.get(ctx, "/v1/admin/audit"+query, &records); err != nil {
		return err
	}

	if asJSON {
		return emitJSON(records)
	}

	if len(records) == 0 {
		fmt.Println("no records")
		return nil
	}

	fmt.Printf("%-20s %-12s %-18s %-24s %-28s %s\n",
		"WHEN", "ACTOR", "ACTION", "REPOSITORY@BRANCH", "PATH", "RULE")

	for _, record := range records {
		where := record.Repository
		if record.Branch != "" {
			where += "@" + record.Branch
		}

		fmt.Printf("%-20s %-12s %-18s %-24s %-28s %s\n",
			record.OccurredAt.Format("2006-01-02 15:04:05"),
			record.Actor, record.Action, where, record.Path, record.RuleID)
	}

	return nil
}

func emitJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")

	return encoder.Encode(value)
}

// formatDuration renders milliseconds the way an operator scans them.
func formatDuration(ms int64) string {
	switch {
	case ms == 0:
		return "-"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm%02ds", ms/60_000, (ms%60_000)/1000)
	}
}
