// Package github is a GitHub adapter: a queue.Forge over the REST API, a
// Checks reporter, and a webhook handler that turns pull_request and push
// events into queue inputs. It is tested only against httptest fakes.
//
// The token is read from the GITHUB_TOKEN environment variable by
// FromEnv and is never logged or included in errors.
//
// Candidate commits are not built through the API (merge commits made by
// GitHub are not deterministic). The intended deployment builds them in a
// local mirror with forge/localgit, pushes them to refs/heads/greenline/*,
// and then calls Advance, which moves main by a fast-forward-only ref
// update.
package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/xartus2004/greenline/queue"
)

// Client talks to the GitHub REST API for one repository.
type Client struct {
	BaseURL string // https://api.github.com (or a fake)
	Owner   string
	Repo    string
	Branch  string // default "main"
	HTTP    *http.Client
	token   string
}

// New returns a client with an explicit token (may be empty for fakes).
func New(baseURL, owner, repo, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Owner: owner, Repo: repo, Branch: "main",
		HTTP: &http.Client{Timeout: 30 * time.Second}, token: token}
}

// FromEnv returns a client for api.github.com using GITHUB_TOKEN.
func FromEnv(owner, repo string) (*Client, error) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		return nil, errors.New("github: GITHUB_TOKEN is not set")
	}
	return New("https://api.github.com", owner, repo, tok), nil
}

// APIError is a non-2xx response. It never contains the token.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		json.Unmarshal(b, &m)
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: m.Message}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) repoPath() string { return "/repos/" + c.Owner + "/" + c.Repo }

// Head implements queue.Forge.
func (c *Client) Head() (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := c.do(context.Background(), http.MethodGet, c.repoPath()+"/git/ref/heads/"+c.Branch, nil, &ref)
	return ref.Object.SHA, err
}

// Advance implements queue.Forge. It checks that next's only parent is old,
// then updates the branch with force=false. GitHub rejects a non-fast-
// forward update, and since next's parent is old, the update can only
// succeed while main is still old: that makes it a compare-and-swap.
func (c *Client) Advance(old, next string) error {
	ctx := context.Background()
	var commit struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := c.do(ctx, http.MethodGet, c.repoPath()+"/git/commits/"+next, nil, &commit); err != nil {
		return err
	}
	if len(commit.Parents) != 1 || commit.Parents[0].SHA != old {
		return fmt.Errorf("github: refusing to advance %s to %s: not a single-parent child", c.Branch, next)
	}
	err := c.do(ctx, http.MethodPatch, c.repoPath()+"/git/refs/heads/"+c.Branch, map[string]any{"sha": next, "force": false}, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
		return queue.ErrStale
	}
	return err
}

// Contains implements queue.Forge: commit is an ancestor of (or equal to)
// the branch head.
func (c *Client) Contains(commit string) (bool, error) {
	var cmp struct {
		Status string `json:"status"`
	}
	err := c.do(context.Background(), http.MethodGet, c.repoPath()+"/compare/"+commit+"..."+c.Branch, nil, &cmp)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cmp.Status == "ahead" || cmp.Status == "identical", nil
}

// CheckRun reports queue status on a PR head commit.
type CheckRun struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`               // queued, in_progress, completed
	Conclusion string `json:"conclusion,omitempty"` // success, failure, cancelled
	Output     *struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output,omitempty"`
}

// ReportPR creates a check run reflecting the PR's queue state.
func (c *Client) ReportPR(ctx context.Context, p queue.PR) error {
	cr := CheckRun{Name: "greenline", HeadSHA: p.Head}
	switch p.State {
	case queue.Queued:
		cr.Status = "queued"
	case queue.Testing:
		cr.Status = "in_progress"
	default:
		cr.Status = "completed"
		cr.Conclusion = map[queue.State]string{queue.Merged: "success", queue.Ejected: "failure", queue.Cancelled: "cancelled"}[p.State]
		title := string(p.State)
		summary := fmt.Sprintf("Merged in %s.", p.Commit)
		if p.State != queue.Merged {
			summary = fmt.Sprintf("Reason: %s. Failed: %s", p.Reason, strings.Join(p.FailedTest, ", "))
		}
		cr.Output = &struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
		}{title, summary}
	}
	return c.do(ctx, http.MethodPost, c.repoPath()+"/check-runs", cr, nil)
}

var _ queue.Forge = (*Client)(nil)

// ------------------------------------------------------------- webhooks

// Queue is what the webhook handler drives.
type Queue interface {
	EnqueueSHA(ctx context.Context, id queue.PRID, head string) error
	Remove(ctx context.Context, id queue.PRID) error
	MainMoved(ctx context.Context) error
}

// Webhook verifies and dispatches GitHub webhook deliveries. A PR enters
// the queue when it carries Label; a push to it updates it; closing it or
// removing the label takes it out; a push to Branch tells the queue main
// moved.
type Webhook struct {
	Secret []byte
	Label  string // default "merge-queue"
	Branch string // default "main"
	Queue  Queue
}

// Verify checks X-Hub-Signature-256 in constant time.
func Verify(secret, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(m.Sum(nil), want)
}

type prEvent struct {
	Action      string `json:"action"`
	Number      int64  `json:"number"`
	PullRequest struct {
		State string `json:"state"`
		Head  struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"pull_request"`
	Label *struct {
		Name string `json:"name"`
	} `json:"label"`
}

func (w *Webhook) label() string {
	if w.Label == "" {
		return "merge-queue"
	}
	return w.Label
}

func (w *Webhook) branch() string {
	if w.Branch == "" {
		return "main"
	}
	return w.Branch
}

func (w *Webhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		http.Error(rw, "read error", http.StatusBadRequest)
		return
	}
	if !Verify(w.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(rw, "bad signature", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
	case "push":
		var ev struct {
			Ref string `json:"ref"`
		}
		if json.Unmarshal(body, &ev) == nil && ev.Ref == "refs/heads/"+w.branch() {
			err = w.Queue.MainMoved(ctx)
		}
	case "pull_request":
		var ev prEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(rw, "bad payload", http.StatusBadRequest)
			return
		}
		err = w.handlePR(ctx, ev)
	}
	if err != nil {
		http.Error(rw, "queue error", http.StatusInternalServerError)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

func (w *Webhook) handlePR(ctx context.Context, ev prEvent) error {
	if ev.PullRequest.Base.Ref != w.branch() {
		return nil
	}
	id := queue.PRID(ev.Number)
	labeled := false
	for _, l := range ev.PullRequest.Labels {
		if l.Name == w.label() {
			labeled = true
		}
	}
	switch ev.Action {
	case "closed":
		return ignoreNotQueued(w.Queue.Remove(ctx, id))
	case "unlabeled":
		if ev.Label != nil && ev.Label.Name == w.label() {
			return ignoreNotQueued(w.Queue.Remove(ctx, id))
		}
	case "labeled", "synchronize", "reopened", "opened":
		if labeled && ev.PullRequest.State == "open" {
			return w.Queue.EnqueueSHA(ctx, id, ev.PullRequest.Head.SHA)
		}
	}
	return nil
}

func ignoreNotQueued(err error) error {
	if err != nil && strings.Contains(err.Error(), "not queued") {
		return nil
	}
	return err
}
