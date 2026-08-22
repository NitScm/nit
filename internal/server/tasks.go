package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/blob"
	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/pkg/protocol"
)

// handleUploadBlob accepts a patch payload.
//
// Uploading is unprivileged: writing bytes into a content-addressed store
// reveals nothing and grants nothing. Reading is the operation that needs
// authorization, and it goes through the task that produced the patch.
func (s *Server) handleUploadBlob(w http.ResponseWriter, r *http.Request) error {
	expected := r.Header.Get("X-Nit-Digest")

	descriptor, err := s.deps.Blobs.Put(r.Context(), r.Body, expected, s.cfg.MaxPatchBytes)

	switch {
	case errors.Is(err, blob.ErrTooLarge):
		return fail(http.StatusRequestEntityTooLarge, protocol.CodePatchTooLarge,
			"the payload exceeds the %d byte limit", s.cfg.MaxPatchBytes)

	case errors.Is(err, blob.ErrDigestMismatch):
		return fail(http.StatusBadRequest, "digest_mismatch",
			"the uploaded bytes do not match the announced digest")

	case err != nil:
		return err
	}

	writeJSON(w, http.StatusCreated, protocol.Blob{
		Digest:   descriptor.Digest,
		Size:     descriptor.Size,
		Encoding: protocol.Encoding(r.Header.Get("X-Nit-Encoding")),
	})

	return nil
}

// handleGetTask returns a task's public view.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) error {
	task, err := s.ownedTask(r)
	if err != nil {
		return err
	}

	view, err := s.taskView(r.Context(), task)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, view)

	return nil
}

// handleTaskEvents holds a long poll open until the task changes state.
//
// This is how a waiting CLI learns its task finished, and it is deliberately
// the client that holds the connection: a developer machine sits behind NAT and
// is not addressable, so a design where the server calls back would need
// tunnels or an agent. A client that cannot hold a connection open falls back
// to polling this same endpoint, which returns immediately once the state has
// moved.
func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) error {
	task, err := s.ownedTask(r)
	if err != nil {
		return err
	}

	ctx := r.Context()

	deadline := time.Now().Add(s.cfg.EventMaxWait)
	initial := task.State

	ticker := time.NewTicker(s.cfg.EventPollInterval)
	defer ticker.Stop()

	for {
		if task.State != initial || task.State.Terminal() {
			return s.writeEvent(ctx, w, task)
		}

		if time.Now().After(deadline) {
			// The long poll timed out with nothing to report. The client
			// reconnects; saying so explicitly is better than an empty body it
			// has to interpret.
			return s.writeEvent(ctx, w, task)
		}

		select {
		case <-ctx.Done():
			// The client hung up. Nothing to report to nobody.
			return nil

		case <-ticker.C:
		}

		task, err = s.deps.Store.Tasks().ByID(ctx, task.ID)
		if err != nil {
			return err
		}
	}
}

func (s *Server) writeEvent(ctx context.Context, w http.ResponseWriter, task *store.Task) error {
	view, err := s.taskView(ctx, task)
	if err != nil {
		return err
	}

	event := protocol.Event{
		TaskID: string(task.ID),
		State:  task.State,
		At:     s.deps.Now(),
	}
	if task.State.Terminal() {
		event.Task = view
	}

	writeJSON(w, http.StatusOK, event)

	return nil
}

// handleTaskPatch streams the patch a task produced.
//
// Patches are fetched through their task rather than from a bare
// /blobs/{digest} endpoint, so authorization is "does this task belong to
// you?" — a question with an answer. Serving content-addressed blobs directly
// would make an unguessable identifier the only thing standing between a
// filtered patch and the people it was filtered for.
func (s *Server) handleTaskPatch(w http.ResponseWriter, r *http.Request) error {
	task, err := s.ownedTask(r)
	if err != nil {
		return err
	}

	if task.State != protocol.TaskSucceeded {
		return fail(http.StatusConflict, "task_not_ready",
			"task %s is %s; no patch is available", task.ID, task.State)
	}

	digest, err := patchDigestOf(task)
	if err != nil {
		return err
	}
	if digest == "" {
		return fail(http.StatusNotFound, "no_patch", "task %s produced no patch", task.ID)
	}

	reader, err := s.deps.Blobs.Get(r.Context(), digest)
	if errors.Is(err, blob.ErrNotFound) {
		// The artifact expired. Saying so lets the client re-request rather
		// than conclude something is broken.
		return fail(http.StatusGone, "patch_expired",
			"the patch for task %s is no longer available; request it again", task.ID)
	}
	if err != nil {
		return err
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Nit-Digest", digest)
	w.WriteHeader(http.StatusOK)

	_, _ = io.Copy(w, reader)

	return nil
}

// ownedTask loads the task named in the path and checks the caller owns it.
func (s *Server) ownedTask(r *http.Request) (*store.Task, error) {
	principal := auth.PrincipalFrom(r.Context())

	id := r.PathValue("id")
	if id == "" {
		return nil, fail(http.StatusBadRequest, "bad_request", "task id is required")
	}

	task, err := s.deps.Store.Tasks().ByID(r.Context(), store.ID(id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fail(http.StatusNotFound, "unknown_task", "unknown task %q", id)
	}
	if err != nil {
		return nil, err
	}

	// 404 rather than 403: a caller has no business learning which task ids
	// exist.
	if task.UserID != principal.User.ID {
		return nil, fail(http.StatusNotFound, "unknown_task", "unknown task %q", id)
	}

	return task, nil
}

// taskView converts a stored task into its public representation.
func (s *Server) taskView(ctx context.Context, task *store.Task) (*protocol.Task, error) {
	repository := ""
	if repo, err := s.deps.Store.Repositories().ByID(ctx, task.RepositoryID); err == nil {
		repository = string(repo.PolicyRepoID)
	}

	view := &protocol.Task{
		ID:         string(task.ID),
		Kind:       task.Kind,
		State:      task.State,
		Repository: repository,
		Branch:     task.Branch,
		CreatedAt:  task.CreatedAt,
		StartedAt:  task.StartedAt,
		FinishedAt: task.FinishedAt,
		Error:      task.Error,
	}

	if task.State == protocol.TaskQueued {
		position, err := s.deps.Queue.Position(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		view.QueuePosition = position
	}

	if task.State == protocol.TaskSucceeded && len(task.Result) > 0 {
		switch task.Kind {
		case protocol.TaskPush:
			var result protocol.PushResult
			if err := json.Unmarshal(task.Result, &result); err != nil {
				return nil, err
			}
			view.PushResult = &result

		case protocol.TaskPull:
			var result protocol.PullResult
			if err := json.Unmarshal(task.Result, &result); err != nil {
				return nil, err
			}
			view.PullResult = &result
		}
	}

	return view, nil
}

// patchDigestOf reads the digest of the patch a completed task produced.
func patchDigestOf(task *store.Task) (string, error) {
	if len(task.Result) == 0 {
		return "", nil
	}

	if task.Kind != protocol.TaskPull {
		return "", nil
	}

	var result protocol.PullResult
	if err := json.Unmarshal(task.Result, &result); err != nil {
		return "", err
	}
	if result.Patch == nil {
		return "", nil
	}

	return result.Patch.Digest, nil
}
