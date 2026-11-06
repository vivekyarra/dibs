package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GitHub talks to the GitHub REST API.
type GitHub struct {
	Repo    string
	Token   string
	BaseURL string
	Client  *http.Client
}

// Token finds a GitHub token: GH_TOKEN, GITHUB_TOKEN, then `gh auth token`.
func Token() (string, error) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, nil
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err == nil && len(bytes.TrimSpace(out)) > 0 {
		return string(bytes.TrimSpace(out)), nil
	}
	return "", errors.New("no GitHub token: set GH_TOKEN or run `gh auth login`")
}

// NewGitHub returns a client for repo (owner/name).
func NewGitHub(repo, token string) *GitHub {
	base := os.Getenv("GITHUB_API_URL")
	if base == "" {
		base = "https://api.github.com"
	}
	return &GitHub{Repo: repo, Token: token, BaseURL: strings.TrimRight(base, "/"), Client: &http.Client{Timeout: 30 * time.Second}}
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s: HTTP %d: %s", e.Path, e.Status, strings.TrimSpace(e.Body))
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// do performs one request with retries on 5xx and rate limits, returning the body and the next-page URL.
func (g *GitHub) do(ctx context.Context, method, path string, in any) ([]byte, string, error) {
	u := path
	if !strings.HasPrefix(u, "http") {
		u = g.BaseURL + path
	}
	var payload []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, "", err
		}
		payload = b
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "dibs")
		if g.Token != "" {
			req.Header.Set("Authorization", "Bearer "+g.Token)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := g.Client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(backoff(attempt, nil))
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			next := ""
			if m := nextLinkRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
				next = m[1]
			}
			return body, next, nil
		}
		lastErr = &APIError{resp.StatusCode, path, string(body)}
		limited := resp.StatusCode == 429 || (resp.StatusCode == 403 && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""))
		if resp.StatusCode >= 500 || limited {
			time.Sleep(backoff(attempt, resp))
			continue
		}
		return nil, "", lastErr
	}
	return nil, "", lastErr
}

func backoff(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n <= 60 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return time.Duration(250*(1<<attempt)) * time.Millisecond
}

func (g *GitHub) getAll(path string, limit int, each func(json.RawMessage) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	next := path
	n := 0
	for next != "" && n < limit {
		body, nxt, err := g.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("github %s: %w", path, err)
		}
		for _, item := range page {
			if err := each(item); err != nil {
				return err
			}
			n++
		}
		next = nxt
	}
	return nil
}

func (g *GitHub) send(method, path string, in any) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _, err := g.do(ctx, method, path, in)
	return err
}

type ghIssue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	State       string `json:"state"`
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
	Labels    []struct{ Name string }  `json:"labels"`
	Assignees []struct{ Login string } `json:"assignees"`
	Milestone *struct {
		DueOn *time.Time `json:"due_on"`
	} `json:"milestone"`
}

func (gi ghIssue) issue() Issue {
	is := Issue{Number: gi.Number, Title: gi.Title, Body: gi.Body, URL: gi.HTMLURL, State: gi.State, Labels: []string{}, Assignees: []string{}}
	for _, l := range gi.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	for _, a := range gi.Assignees {
		is.Assignees = append(is.Assignees, a.Login)
	}
	if gi.Milestone != nil && gi.Milestone.DueOn != nil {
		is.DueOn = *gi.Milestone.DueOn
	}
	return is
}

func (g *GitHub) repoPath(rest string) string { return "/repos/" + g.Repo + rest }

// Issues implements Tracker.
func (g *GitHub) Issues() ([]Issue, error) {
	var out []Issue
	err := g.getAll(g.repoPath("/issues?state=open&per_page=100"), 2000, func(raw json.RawMessage) error {
		var gi ghIssue
		if err := json.Unmarshal(raw, &gi); err != nil {
			return err
		}
		if gi.PullRequest == nil {
			out = append(out, gi.issue())
		}
		return nil
	})
	return out, err
}

// Issue implements Tracker.
func (g *GitHub) Issue(n int) (Issue, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	body, _, err := g.do(ctx, http.MethodGet, g.repoPath(fmt.Sprintf("/issues/%d", n)), nil)
	if err != nil {
		return Issue{}, err
	}
	var gi ghIssue
	if err := json.Unmarshal(body, &gi); err != nil {
		return Issue{}, err
	}
	return gi.issue(), nil
}

type ghPR struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	State     string     `json:"state"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	User      struct{ Login string }
	Head      struct{ Ref string }
}

// PRs implements Tracker: open PRs plus PRs closed in the last 30 days.
func (g *GitHub) PRs() ([]PR, error) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	var out []PR
	add := func(gp ghPR) {
		pr := PR{Number: gp.Number, Title: gp.Title, Body: gp.Body, URL: gp.HTMLURL, Author: gp.User.Login, State: gp.State, HeadRef: gp.Head.Ref}
		if gp.MergedAt != nil {
			pr.Merged, pr.MergedAt = true, *gp.MergedAt
		}
		out = append(out, pr)
	}
	// Every open PR.
	err := g.getAll(g.repoPath("/pulls?state=open&per_page=100"), 2000, func(raw json.RawMessage) error {
		var gp ghPR
		if err := json.Unmarshal(raw, &gp); err != nil {
			return err
		}
		add(gp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Closed PRs, newest activity first, until activity is older than the cutoff.
	// An old PR touched today still sorts first, so skip (don't stop) on close date.
	stop := errors.New("stop")
	err = g.getAll(g.repoPath("/pulls?state=closed&sort=updated&direction=desc&per_page=100"), 1000, func(raw json.RawMessage) error {
		var gp ghPR
		if err := json.Unmarshal(raw, &gp); err != nil {
			return err
		}
		if !gp.UpdatedAt.IsZero() && gp.UpdatedAt.Before(cutoff) {
			return stop
		}
		if gp.ClosedAt != nil && gp.ClosedAt.Before(cutoff) {
			return nil
		}
		add(gp)
		return nil
	})
	if errors.Is(err, stop) {
		err = nil
	}
	return out, err
}

// Reviews returns a PR's reviews and one author entry per review comment.
func (g *GitHub) Reviews(n int) ([]Review, []string, error) {
	var reviews []Review
	err := g.getAll(g.repoPath(fmt.Sprintf("/pulls/%d/reviews?per_page=100", n)), 500, func(raw json.RawMessage) error {
		var r struct {
			User  struct{ Login string }
			State string
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		reviews = append(reviews, Review{Author: r.User.Login, State: r.State})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	var authors []string
	err = g.getAll(g.repoPath(fmt.Sprintf("/pulls/%d/comments?per_page=100", n)), 1000, func(raw json.RawMessage) error {
		var c struct{ User struct{ Login string } }
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		authors = append(authors, c.User.Login)
		return nil
	})
	return reviews, authors, err
}

// EnsureLabels implements Tracker (create, or update colour/description if it exists).
func (g *GitHub) EnsureLabels(ls []Label) error {
	for _, l := range ls {
		err := g.send(http.MethodPost, g.repoPath("/labels"), map[string]string{"name": l.Name, "color": l.Color, "description": l.Description})
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 422 {
			err = g.send(http.MethodPatch, g.repoPath("/labels/"+url.PathEscape(l.Name)), map[string]string{"color": l.Color, "description": l.Description})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// AddLabels implements Tracker.
func (g *GitHub) AddLabels(n int, labels ...string) error {
	return g.send(http.MethodPost, g.repoPath(fmt.Sprintf("/issues/%d/labels", n)), map[string][]string{"labels": labels})
}

// RemoveLabel implements Tracker; a missing label is not an error.
func (g *GitHub) RemoveLabel(n int, label string) error {
	err := g.send(http.MethodDelete, g.repoPath(fmt.Sprintf("/issues/%d/labels/%s", n, url.PathEscape(label))), nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 404 {
		return nil
	}
	return err
}

// Assign implements Tracker.
func (g *GitHub) Assign(n int, login string) error {
	return g.send(http.MethodPost, g.repoPath(fmt.Sprintf("/issues/%d/assignees", n)), map[string][]string{"assignees": {login}})
}

// Comment implements Tracker.
func (g *GitHub) Comment(n int, body string) error {
	return g.send(http.MethodPost, g.repoPath(fmt.Sprintf("/issues/%d/comments", n)), map[string]string{"body": body})
}
