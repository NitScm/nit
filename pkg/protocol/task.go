package protocol

import "time"

// TaskKind is what a queued task does.
type TaskKind string

const (
	TaskPush TaskKind = "push"
	TaskPull TaskKind = "pull"
)

// TaskState is where a task is in its lifecycle.
type TaskState string

const (
	// TaskQueued: accepted and waiting. A push on a busy branch sits here
	// rather than being refused — the queue is what serializes a branch, so a
	// developer never has to retry by hand.
	TaskQueued TaskState = "queued"

	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"

	// TaskCancelled: superseded or cancelled by an operator.
	TaskCancelled TaskState = "cancelled"
)

// Terminal reports whether no further transition is possible.
func (s TaskState) Terminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCancelled:
		return true
	default:
		return false
	}
}

// Task is the public view of a queued operation.
type Task struct {
	ID    string    `json:"id"`
	Kind  TaskKind  `json:"kind"`
	State TaskState `json:"state"`

	Repository string `json:"repository"`
	Branch     string `json:"branch"`

	QueuePosition int `json:"queue_position,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Error is set when State is TaskFailed.
	Error *Error `json:"error,omitempty"`

	// PushResult and PullResult carry the outcome; exactly one is set on a
	// successful task, according to Kind.
	PushResult *PushResult `json:"push_result,omitempty"`
	PullResult *PullResult `json:"pull_result,omitempty"`
}

// PushResult is delivered when a push task completes.
type PushResult struct {
	// UpstreamCommit is the commit created on the forge.
	UpstreamCommit string `json:"upstream_commit"`

	// NextSync is the sync point the client must record. After a push the
	// workspace is projected from the commit that just landed, so the client
	// moves forward without a round trip through pull.
	NextSync SyncToken `json:"next_sync"`

	Report PushReport `json:"report"`
}

// Event is one message of a task's event stream.
//
// The CLI holds this stream open while it waits, which is why the server never
// needs to reach back to a developer machine.
type Event struct {
	TaskID string    `json:"task_id"`
	State  TaskState `json:"state"`
	At     time.Time `json:"at"`

	// Message is human-readable progress ("cloning", "applying patch",
	// "pushing"). Workers emit it so a slow operation does not look hung.
	Message string `json:"message,omitempty"`

	// Task carries the full task once it reaches a terminal state, so the
	// client does not need a follow-up request.
	Task *Task `json:"task,omitempty"`
}

// HTTP routes. They live here so client and server cannot drift apart, and so
// the contract is readable in one place.
const (
	RoutePush       = "/v1/push"
	RoutePull       = "/v1/pull"
	RouteBlobs      = "/v1/blobs"             // POST: upload a patch
	RouteTasks      = "/v1/tasks"             // GET /{id}
	RouteEvents     = "/v1/tasks/{id}/events" // long poll
	RouteTaskPatch  = "/v1/tasks/{id}/patch"  // GET: fetch the patch a task produced
	RouteWorkspaces = "/v1/workspaces"
	RouteWhoAmI     = "/v1/whoami"
	RouteRepos      = "/v1/repositories"
	RouteHealthz    = "/healthz"
)

// TaskPatchPath returns the download path for a task's patch.
//
// Patches are fetched through the task that produced them, not from a
// content-addressed blob endpoint. Authorization is then simply "does this task
// belong to you?", which is checkable; a bare /blobs/{digest} endpoint would
// make an unguessable identifier the only thing standing between a filtered
// patch and someone it was filtered for.
func TaskPatchPath(taskID string) string {
	return RouteTasks + "/" + taskID + "/patch"
}
