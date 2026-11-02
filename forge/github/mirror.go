package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"

	"github.com/xartus2004/greenline/forge/localgit"
	"github.com/xartus2004/greenline/queue"
)

// Mirrored is the forge used by `greenline serve -github`. Candidate commits
// are built deterministically in a local bare mirror (so the runner can use
// it like any localgit repo); Advance pushes the candidate to
// refs/heads/greenline/<sha> on GitHub and then moves main with the API's
// fast-forward-only ref update.
type Mirrored struct {
	API    *Client
	Mirror *localgit.Repo
	Remote string // git URL of the GitHub repo
	// GitEnv carries git credentials via GIT_CONFIG_* variables, so the
	// token never appears in a command line (see GitAuthEnv).
	GitEnv []string
}

// GitAuthEnv returns environment variables that make git send token as
// GitHub's x-access-token basic auth for every HTTPS request.
func GitAuthEnv(token string) []string {
	if token == "" {
		return nil
	}
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + cred}
}

// OpenMirror creates (or reuses) a bare mirror at dir and fetches main.
func OpenMirror(ctx context.Context, api *Client, remote, dir string) (*Mirrored, error) {
	repo, err := localgit.Init(ctx, dir, "")
	if err != nil {
		return nil, err
	}
	m := &Mirrored{API: api, Mirror: repo, Remote: remote, GitEnv: GitAuthEnv(api.token)}
	return m, m.FetchMain(ctx)
}

func (m *Mirrored) git(ctx context.Context, args ...string) error {
	_, err := m.Mirror.Git(ctx, append(os.Environ(), m.GitEnv...), args...)
	if err != nil {
		// Never surface the environment (which holds the credential).
		return fmt.Errorf("github mirror: git %s failed", args[0])
	}
	return nil
}

// FetchMain updates the mirror's main from GitHub.
func (m *Mirrored) FetchMain(ctx context.Context) error {
	b := m.API.branch()
	return m.git(ctx, "fetch", "-q", m.Remote, "+refs/heads/"+b+":refs/heads/main")
}

// FetchPR fetches a pull request's head into refs/heads/pr/<id>.
func (m *Mirrored) FetchPR(ctx context.Context, id queue.PRID) error {
	n := strconv.FormatInt(int64(id), 10)
	return m.git(ctx, "fetch", "-q", m.Remote, "+refs/pull/"+n+"/head:refs/heads/pr/"+n)
}

// Head implements queue.Forge (GitHub is the source of truth). The mirror is
// refreshed if it does not yet have that commit.
func (m *Mirrored) Head() (string, error) {
	h, err := m.API.Head()
	if err != nil {
		return "", err
	}
	if _, err := m.Mirror.Git(context.Background(), nil, "cat-file", "-e", h+"^{commit}"); err != nil {
		if err := m.FetchMain(context.Background()); err != nil {
			return "", err
		}
	}
	return h, nil
}

// Advance implements queue.Forge.
func (m *Mirrored) Advance(old, next string) error {
	ctx := context.Background()
	if err := m.git(ctx, "push", "-q", "-f", m.Remote, next+":refs/heads/greenline/"+next); err != nil {
		return err
	}
	if err := m.API.Advance(old, next); err != nil {
		return err
	}
	// Keep the mirror's main in step (best effort; FetchMain also fixes it)
	// and drop the temporary branch.
	m.Mirror.Git(ctx, nil, "update-ref", "refs/heads/main", next)
	m.git(ctx, "push", "-q", m.Remote, ":refs/heads/greenline/"+next)
	return nil
}

// Contains implements queue.Forge.
func (m *Mirrored) Contains(commit string) (bool, error) { return m.API.Contains(commit) }

func (c *Client) branch() string {
	if c.Branch == "" {
		return "main"
	}
	return c.Branch
}

var _ queue.Forge = (*Mirrored)(nil)
