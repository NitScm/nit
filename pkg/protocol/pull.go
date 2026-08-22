package protocol

// PullRequest is sent by the CLI to fetch upstream changes.
type PullRequest struct {
	ProtocolVersion string `json:"protocol_version"`

	RequestID string `json:"request_id"`

	Repository string `json:"repository"`
	Branch     string `json:"branch"`

	Workspace WorkspaceID `json:"workspace"`

	// Sync is the client's current sync point. An empty token asks for a full
	// filtered snapshot, which is what "nit clone" does.
	Sync SyncToken `json:"sync"`
}

// PullResponse acknowledges the request. The patch is produced asynchronously
// by a worker: computing it requires a clone and a filtered diff, which is far
// too slow to hold an HTTP request open.
type PullResponse struct {
	TaskID string `json:"task_id"`

	// UpToDate is true when the sync point already matches upstream HEAD. No
	// task is queued in that case and TaskID is empty.
	UpToDate bool `json:"up_to_date"`
}

// PullResult is delivered when the task completes, either through the event
// stream or by polling the task.
//
// Note the direction of the final transfer: the server never connects to the
// developer's machine. A laptop sits behind NAT and a firewall and is not
// addressable; the CLI holds a long-poll open on the task instead, and fetches
// the blob itself once told it is ready. That inversion is what removes any
// need for tunnels, open ports or agents on developer machines.
type PullResult struct {
	// Patch is the filtered diff to apply locally. It is absent when the
	// filtered diff turned out to be empty — every upstream change was
	// withheld — in which case only the sync point moves.
	Patch *Blob `json:"patch,omitempty"`

	// NextSync is the sync point the client must record once the patch is
	// applied. Storing it before applying, or failing to store it after, is
	// what makes a workspace drift.
	NextSync SyncToken `json:"next_sync"`

	Report PullReport `json:"report"`
}

// PullReport tells the developer what they received and what was withheld.
type PullReport struct {
	PolicyVersion string `json:"policy_version"`

	FilesTotal     int `json:"files_total"`
	FilesDelivered int `json:"files_delivered"`

	// FilesWithheld counts sections removed because the developer may not read
	// the paths they touch.
	//
	// The count is reported without the paths: naming them would leak the very
	// structure the read rules exist to hide. It is reported at all because a
	// developer who does not know something was withheld will mistake a missing
	// file for a deleted one.
	FilesWithheld int `json:"files_withheld"`

	// UpstreamCommit is the commit the workspace is now projected from,
	// surfaced so support can correlate a workspace with upstream history.
	UpstreamCommit string `json:"upstream_commit"`
}
