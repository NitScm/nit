package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// The operations API: what nitctl and the web console read.
//
// It is a separate surface from the developer API, under /v1/admin, and it is
// read-only. Everything that changes authorization goes through the policy
// bundle — reviewed, versioned, rolled back like code — so a console that could
// write rules would be a second, unreviewed path to the same decisions.
//
// Access is by group membership, named in the server configuration rather than
// in the bundle. An operator locked out by a bad rule still has to be able to
// look at why.

// adminOnly restricts a handler to members of the configured admin groups.
func (s *Server) adminOnly(h handlerFunc) http.Handler {
	return s.authenticated(func(w http.ResponseWriter, r *http.Request) error {
		principal := auth.PrincipalFrom(r.Context())

		if !s.isAdmin(principal) {
			// 404 rather than 403: the existence of an operations API is not
			// something an ordinary developer needs confirmed.
			return fail(http.StatusNotFound, "not_found", "not found")
		}

		return h(w, r)
	})
}

func (s *Server) isAdmin(principal *auth.Principal) bool {
	for _, group := range s.cfg.AdminGroups {
		if principal.Subject.InGroup(group) {
			return true
		}
	}

	return false
}

// TaskView is a task as the operations API reports it.
//
// It carries more than the developer view — the owner, the attempt count, the
// lease holder — because the questions an operator asks are different: not
// "did my push land?" but "why is this branch stuck?".
type TaskView struct {
	ID    string             `json:"id"`
	Kind  protocol.TaskKind  `json:"kind"`
	State protocol.TaskState `json:"state"`

	User       string `json:"user"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`

	Attempts      int    `json:"attempts"`
	QueuePosition int    `json:"queue_position,omitempty"`
	LeaseHolder   string `json:"lease_holder,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// DurationMs is how long the task ran, or has been running.
	DurationMs int64 `json:"duration_ms,omitempty"`

	Error *protocol.Error `json:"error,omitempty"`
}

// handleAdminTasks lists tasks.
func (s *Server) handleAdminTasks(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	query := r.URL.Query()

	filter := store.TaskFilter{
		Tenant: tenantOf(ctx),
		Branch: query.Get("branch"),
		Limit:  intParam(query.Get("limit"), 50, 500),
	}

	for _, state := range query["state"] {
		filter.States = append(filter.States, protocol.TaskState(state))
	}
	for _, kind := range query["kind"] {
		filter.Kinds = append(filter.Kinds, protocol.TaskKind(kind))
	}

	if name := query.Get("repository"); name != "" {
		repo, err := s.resolveRepository(ctx, name)
		if err != nil {
			return err
		}
		filter.RepositoryID = repo.ID
	}

	tasks, err := s.deps.Store.Tasks().List(ctx, filter)
	if err != nil {
		return err
	}

	views := make([]TaskView, 0, len(tasks))

	// Names are resolved once per id rather than per row: a page of fifty tasks
	// on one repository would otherwise be fifty identical lookups.
	users := newNameCache()
	repos := newNameCache()

	for _, task := range tasks {
		view := TaskView{
			ID:         string(task.ID),
			Kind:       task.Kind,
			State:      task.State,
			Branch:     task.Branch,
			Attempts:   task.Attempts,
			CreatedAt:  task.CreatedAt,
			StartedAt:  task.StartedAt,
			FinishedAt: task.FinishedAt,
			Error:      task.Error,
			User:       users.get(string(task.UserID), func() string { return s.userName(ctx, task.UserID) }),
			Repository: repos.get(string(task.RepositoryID), func() string { return s.repositoryName(ctx, task.RepositoryID) }),
			DurationMs: durationOf(task, s.deps.Now()),
		}

		if task.Lease != nil {
			view.LeaseHolder = task.Lease.Holder
		}

		if task.State == protocol.TaskQueued {
			position, err := s.deps.Queue.Position(ctx, task.ID)
			if err != nil {
				return err
			}
			view.QueuePosition = position
		}

		views = append(views, view)
	}

	writeJSON(w, http.StatusOK, views)

	return nil
}

// AdminTaskDetail adds the task's payload and result to its summary.
type AdminTaskDetail struct {
	TaskView

	// Payload and Result are the raw specs, so an operator can see exactly what
	// a worker was told to do and what it reported.
	Payload json.RawMessage `json:"payload,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

func (s *Server) handleAdminTask(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	task, err := s.deps.Store.Tasks().ByID(ctx, store.ID(r.PathValue("id")))
	if err != nil {
		return fail(http.StatusNotFound, "unknown_task", "unknown task")
	}

	detail := AdminTaskDetail{
		TaskView: TaskView{
			ID:         string(task.ID),
			Kind:       task.Kind,
			State:      task.State,
			Branch:     task.Branch,
			Attempts:   task.Attempts,
			CreatedAt:  task.CreatedAt,
			StartedAt:  task.StartedAt,
			FinishedAt: task.FinishedAt,
			Error:      task.Error,
			User:       s.userName(ctx, task.UserID),
			Repository: s.repositoryName(ctx, task.RepositoryID),
			DurationMs: durationOf(task, s.deps.Now()),
		},
		Payload: json.RawMessage(task.Payload),
		Result:  json.RawMessage(task.Result),
	}

	if task.Lease != nil {
		detail.LeaseHolder = task.Lease.Holder
	}

	writeJSON(w, http.StatusOK, detail)

	return nil
}

// AuditView is one audit record as the operations API reports it.
type AuditView struct {
	ID         int64     `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`

	Actor  string `json:"actor"`
	Action string `json:"action"`

	Repository string `json:"repository,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Path       string `json:"path,omitempty"`

	Effect string `json:"effect,omitempty"`
	Reason string `json:"reason,omitempty"`
	RuleID string `json:"rule_id,omitempty"`
	Guard  string `json:"guard,omitempty"`

	PolicyVersion string `json:"policy_version"`
	RequestID     string `json:"request_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
}

// handleAdminAudit queries the audit log.
//
// This is the endpoint the whole product exists to make possible: who could do
// what, when, and under which rules.
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	query := r.URL.Query()

	q := store.AuditQuery{
		Tenant:    tenantOf(ctx),
		RequestID: query.Get("request_id"),
		Limit:     intParam(query.Get("limit"), 100, 1000),
	}

	if since := query.Get("since"); since != "" {
		parsed, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return fail(http.StatusBadRequest, "bad_request", "since must be an RFC3339 timestamp")
		}
		q.Since = parsed
	}
	if until := query.Get("until"); until != "" {
		parsed, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return fail(http.StatusBadRequest, "bad_request", "until must be an RFC3339 timestamp")
		}
		q.Until = parsed
	}

	if name := query.Get("repository"); name != "" {
		repo, err := s.resolveRepository(ctx, name)
		if err != nil {
			return err
		}
		q.RepositoryID = repo.ID
	}

	if user := query.Get("user"); user != "" {
		record, err := s.deps.Store.Users().ByPolicyID(ctx, tenantOf(ctx), policy.UserID(user))
		if err != nil {
			return fail(http.StatusNotFound, "unknown_user", "unknown user %q", user)
		}
		q.ActorUserID = record.ID
	}

	records, err := s.deps.Store.Audit().Query(ctx, q)
	if err != nil {
		return err
	}

	repos := newNameCache()
	views := make([]AuditView, 0, len(records))

	for _, record := range records {
		views = append(views, AuditView{
			ID:            record.ID,
			OccurredAt:    record.OccurredAt,
			Actor:         record.ActorLabel,
			Action:        record.Action,
			Repository:    repos.get(string(record.RepositoryID), func() string { return s.repositoryName(ctx, record.RepositoryID) }),
			Branch:        record.Branch,
			Path:          record.Path,
			Effect:        string(record.Effect),
			Reason:        string(record.Reason),
			RuleID:        record.RuleID,
			Guard:         record.Guard,
			PolicyVersion: record.PolicyVersion,
			RequestID:     record.RequestID,
			TaskID:        string(record.TaskID),
		})
	}

	writeJSON(w, http.StatusOK, views)

	return nil
}

// Stats is the dashboard summary.
type Stats struct {
	PolicyVersion string `json:"policy_version"`

	// Tasks counts tasks by state.
	Tasks map[string]int `json:"tasks"`

	// QueueDepth is how many tasks are waiting, and BusyBranches how many are
	// currently held by a running push. Together they answer the only question
	// an operator has about the queue: is anything stuck?
	QueueDepth   int `json:"queue_depth"`
	BusyBranches int `json:"busy_branches"`

	Repositories int `json:"repositories"`
	Users        int `json:"users"`
	Groups       int `json:"groups"`

	// RecentDenials counts refused paths over the last day, the number that
	// tells an operator whether the policy is fighting the team.
	RecentDenials int `json:"recent_denials"`
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	current := s.deps.Policy.Current()

	stats := Stats{
		PolicyVersion: current.Version(),
		Tasks:         map[string]int{},
		Repositories:  len(current.Repositories()),
	}

	for _, state := range []protocol.TaskState{
		protocol.TaskQueued, protocol.TaskRunning,
		protocol.TaskSucceeded, protocol.TaskFailed, protocol.TaskCancelled,
	} {
		tasks, err := s.deps.Store.Tasks().List(ctx, store.TaskFilter{
			Tenant: tenantOf(ctx),
			States: []protocol.TaskState{state},
			Limit:  1000,
		})
		if err != nil {
			return err
		}

		stats.Tasks[string(state)] = len(tasks)

		switch state {
		case protocol.TaskQueued:
			stats.QueueDepth = len(tasks)

		case protocol.TaskRunning:
			// A running task with a partition key holds a branch; counting the
			// distinct keys is what "how many branches are blocked" means.
			branches := map[string]struct{}{}

			for _, task := range tasks {
				if task.PartitionKey != "" {
					branches[task.PartitionKey] = struct{}{}
				}
			}

			stats.BusyBranches = len(branches)
		}
	}

	denials, err := s.deps.Store.Audit().Query(ctx, store.AuditQuery{
		Tenant: tenantOf(ctx),
		Since:  s.deps.Now().Add(-24 * time.Hour),
		Limit:  1000,
	})
	if err != nil {
		return err
	}

	for _, record := range denials {
		if record.Effect == policy.EffectDeny {
			stats.RecentDenials++
		}
	}

	stats.Users, stats.Groups = s.countPrincipals(current)

	writeJSON(w, http.StatusOK, stats)

	return nil
}

// PolicyView describes the bundle in force, so the console can show what the
// rules actually are without anyone reading YAML on a server.
type PolicyView struct {
	Version      string           `json:"version"`
	Repositories []PolicyRepoView `json:"repositories"`
}

// PolicyRepoView is one repository and its rules.
type PolicyRepoView struct {
	ID            string           `json:"id"`
	Remote        string           `json:"remote"`
	Forge         string           `json:"forge"`
	DefaultBranch string           `json:"default_branch"`
	Rules         []PolicyRuleView `json:"rules"`
}

// PolicyRuleView is a rule, flattened.
type PolicyRuleView struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Except      []string `json:"except,omitempty"`
	Paths       []string `json:"paths"`
	Refs        []string `json:"refs,omitempty"`
	Actions     []string `json:"actions"`
	Effect      string   `json:"effect"`
	Description string   `json:"description,omitempty"`
}

func (s *Server) handleAdminPolicy(w http.ResponseWriter, r *http.Request) error {
	current := s.deps.Policy.Current()

	view := PolicyView{Version: current.Version()}

	for _, repo := range current.Repositories() {
		repoView := PolicyRepoView{
			ID:            string(repo.ID),
			Remote:        repo.Remote,
			Forge:         repo.Forge,
			DefaultBranch: repo.DefaultBranch,
		}

		for _, rule := range current.Rules(repo.ID) {
			ruleView := PolicyRuleView{
				ID:          rule.ID,
				Subject:     rule.Subject.String(),
				Effect:      string(rule.Effect),
				Description: rule.Description,
			}

			for _, except := range rule.Except {
				ruleView.Except = append(ruleView.Except, except.String())
			}
			for _, pattern := range rule.Paths {
				ruleView.Paths = append(ruleView.Paths, pattern.String())
			}
			for _, pattern := range rule.Refs {
				ruleView.Refs = append(ruleView.Refs, pattern.String())
			}
			for _, action := range rule.Actions {
				ruleView.Actions = append(ruleView.Actions, string(action))
			}

			repoView.Rules = append(repoView.Rules, ruleView)
		}

		view.Repositories = append(view.Repositories, repoView)
	}

	writeJSON(w, http.StatusOK, view)

	return nil
}

// ---------------------------------------------------------------------------

// nameCache memoizes id-to-name lookups for the duration of one response.
type nameCache struct{ entries map[string]string }

func newNameCache() *nameCache { return &nameCache{entries: map[string]string{}} }

func (c *nameCache) get(id string, resolve func() string) string {
	if id == "" {
		return ""
	}
	if name, ok := c.entries[id]; ok {
		return name
	}

	name := resolve()
	c.entries[id] = name

	return name
}

func (s *Server) userName(ctx context.Context, id store.ID) string {
	user, err := s.deps.Store.Users().ByID(ctx, id)
	if err != nil {
		return string(id)
	}
	return string(user.PolicyUserID)
}

func (s *Server) repositoryName(ctx context.Context, id store.ID) string {
	repo, err := s.deps.Store.Repositories().ByID(ctx, id)
	if err != nil {
		return string(id)
	}
	return string(repo.PolicyRepoID)
}

func (s *Server) countPrincipals(p *policy.Policy) (users, groups int) {
	seenUsers := map[policy.UserID]struct{}{}
	seenGroups := map[policy.GroupID]struct{}{}

	for _, repo := range p.Repositories() {
		for _, rule := range p.Rules(repo.ID) {
			switch rule.Subject.Type {
			case policy.SubjectTypeUser:
				seenUsers[policy.UserID(rule.Subject.ID)] = struct{}{}
			case policy.SubjectTypeGroup:
				seenGroups[policy.GroupID(rule.Subject.ID)] = struct{}{}
			}
		}
	}

	return len(seenUsers), len(seenGroups)
}

// durationOf reports how long a task ran, or has been running.
func durationOf(task *store.Task, now time.Time) int64 {
	if task.StartedAt == nil {
		return 0
	}

	end := now
	if task.FinishedAt != nil {
		end = *task.FinishedAt
	}

	return end.Sub(*task.StartedAt).Milliseconds()
}

// intParam parses a bounded integer query parameter.
func intParam(raw string, fallback, max int) int {
	if raw == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	if parsed > max {
		return max
	}

	return parsed
}
