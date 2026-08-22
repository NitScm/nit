// Package client speaks the nit API.
//
// It is a package rather than code inside cmd/nit for one reason that matters:
// the end-to-end tests drive this client against a real server and a real
// worker. A CLI whose HTTP handling lives in main() can only be tested by
// running the binary, so in practice it is not tested at all — and the client is
// where a protocol mismatch shows up first.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/NitScm/nit/pkg/protocol"
)

// Client calls a nit server.
type Client struct {
	// BaseURL is the server root, without a trailing slash.
	BaseURL string

	// Token authenticates every request.
	Token string

	HTTP *http.Client
}

// New returns a client for a server.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,

		// No global timeout: the event endpoint is a long poll held open by
		// design, and a blanket deadline would cut off exactly the request that
		// is working correctly. Per-call deadlines come from the context.
		HTTP: &http.Client{},
	}
}

// Error is a failure reported by the server. Callers match on the code.
type Error = protocol.Error

// IsCode reports whether err is a server error with the given code.
func IsCode(err error, code string) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Code == code
}

// do performs a request and decodes the response.
//
// body may be nil, a []byte (sent verbatim), or any value to marshal as JSON.
// out may be nil to discard the response body.
func (c *Client) do(ctx context.Context, method, path string, body, out any, headers map[string]string) error {
	var reader io.Reader

	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	case io.Reader:
		reader = b
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}

	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}

	if out == nil {
		return nil
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// decodeError turns an error response into a typed error.
//
// A server that answered with something other than the documented envelope —
// a proxy error page, a load balancer's HTML — must still produce a usable
// message rather than a JSON decoding failure.
func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var apiErr Error
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Code != "" {
		return &apiErr
	}

	message := strings.TrimSpace(string(body))
	if message == "" {
		message = resp.Status
	}
	if len(message) > 300 {
		message = message[:300] + "…"
	}

	return &Error{Code: "http_" + fmt.Sprint(resp.StatusCode), Message: message}
}

// WhoAmI describes the authenticated caller.
type WhoAmI struct {
	User          string   `json:"user"`
	Email         string   `json:"email"`
	Groups        []string `json:"groups"`
	PolicyVersion string   `json:"policy_version"`
}

// WhoAmI verifies the credential and returns the identity behind it.
func (c *Client) WhoAmI(ctx context.Context) (*WhoAmI, error) {
	var out WhoAmI
	if err := c.do(ctx, http.MethodGet, protocol.RouteWhoAmI, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Repository is a repository the caller can read something in.
type Repository struct {
	ID            string `json:"id"`
	DefaultBranch string `json:"default_branch"`
	Forge         string `json:"forge"`
}

// Repositories lists what the caller can see.
func (c *Client) Repositories(ctx context.Context) ([]Repository, error) {
	var out []Repository
	if err := c.do(ctx, http.MethodGet, protocol.RouteRepos, nil, &out, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// Workspace is a registered checkout.
type Workspace struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// CreateWorkspace registers a checkout and returns its id.
func (c *Client) CreateWorkspace(ctx context.Context, label string) (*Workspace, error) {
	var out Workspace

	body := struct {
		Label string `json:"label"`
	}{Label: label}

	if err := c.do(ctx, http.MethodPost, protocol.RouteWorkspaces, body, &out, nil); err != nil {
		return nil, err
	}

	return &out, nil
}

// UploadPatch stores a compressed patch and returns its descriptor.
func (c *Client) UploadPatch(ctx context.Context, compressed []byte, digest string) (protocol.Blob, error) {
	var out protocol.Blob

	headers := map[string]string{"Content-Type": "application/octet-stream"}
	if digest != "" {
		// Announcing the digest lets the server refuse a corrupted upload
		// instead of storing it under a name that does not describe it.
		headers["X-Nit-Digest"] = digest
	}

	if err := c.do(ctx, http.MethodPost, protocol.RouteBlobs, compressed, &out, headers); err != nil {
		return protocol.Blob{}, err
	}

	return out, nil
}

// Push submits a change.
func (c *Client) Push(ctx context.Context, req protocol.PushRequest) (*protocol.PushResponse, error) {
	req.ProtocolVersion = protocol.Version

	var out protocol.PushResponse
	if err := c.do(ctx, http.MethodPost, protocol.RoutePush, req, &out, nil); err != nil {
		return nil, err
	}

	return &out, nil
}

// Pull requests upstream changes.
func (c *Client) Pull(ctx context.Context, req protocol.PullRequest) (*protocol.PullResponse, error) {
	req.ProtocolVersion = protocol.Version

	var out protocol.PullResponse
	if err := c.do(ctx, http.MethodPost, protocol.RoutePull, req, &out, nil); err != nil {
		return nil, err
	}

	return &out, nil
}

// Task returns a task's current state.
func (c *Client) Task(ctx context.Context, id string) (*protocol.Task, error) {
	var out protocol.Task
	if err := c.do(ctx, http.MethodGet, protocol.RouteTasks+"/"+id, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitForTask blocks until a task reaches a terminal state.
//
// It holds a long poll open on the event endpoint rather than polling on a
// timer: the server never connects back to a developer's machine — it is behind
// NAT — so this is how a waiting CLI learns its task finished. Each poll returns
// either a state change or a timeout, and the loop simply reconnects.
//
// onProgress, when set, is called on every state change so the CLI can show
// something while a clone runs.
func (c *Client) WaitForTask(ctx context.Context, id string, onProgress func(protocol.TaskState, int)) (*protocol.Task, error) {
	var last protocol.TaskState

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var event protocol.Event

		err := c.do(ctx, http.MethodGet, "/v1/tasks/"+id+"/events", nil, &event, nil)
		if err != nil {
			return nil, err
		}

		if event.Task != nil && event.Task.State.Terminal() {
			return event.Task, nil
		}

		if event.State != last {
			last = event.State

			if onProgress != nil {
				onProgress(event.State, queuePositionOf(event))
			}
		}

		if event.State.Terminal() {
			// Terminal without a body: ask for the task itself.
			return c.Task(ctx, id)
		}
	}
}

func queuePositionOf(event protocol.Event) int {
	if event.Task != nil {
		return event.Task.QueuePosition
	}
	return 0
}

// FetchPatch downloads the patch a task produced.
//
// The download goes through the task, not through a content-addressed blob
// endpoint: authorization is then "does this task belong to you?" rather than
// "do you know this digest?".
func (c *Client) FetchPatch(ctx context.Context, taskID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+protocol.TaskPatchPath(taskID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, decodeError(resp)
	}

	return io.ReadAll(resp.Body)
}

// Health is the unauthenticated liveness response.
type Health struct {
	Status          string `json:"status"`
	ProtocolVersion string `json:"protocol_version"`
	PolicyVersion   string `json:"policy_version"`
}

// Health checks that a server is reachable, before asking for a credential.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var out Health
	if err := c.do(ctx, http.MethodGet, protocol.RouteHealthz, nil, &out, nil); err != nil {
		return nil, err
	}

	return &out, nil
}
