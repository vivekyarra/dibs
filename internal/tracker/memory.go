package tracker

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-process tracker. With a path it persists to a JSON file,
// which is how the offline demo and tests model GitHub.
type Memory struct {
	mu       sync.Mutex
	path     string
	IssuesM  map[int]*Issue `json:"issues"`
	PRsM     map[int]*PR    `json:"prs"`
	LabelsM  map[string]Label
	Comments map[int][]string `json:"comments"`
	Repo     string           `json:"repo"`
	NextNum  int              `json:"next"`
}

// NewMemory returns an empty tracker for repo.
func NewMemory(repo string) *Memory {
	return &Memory{IssuesM: map[int]*Issue{}, PRsM: map[int]*PR{}, LabelsM: map[string]Label{}, Comments: map[int][]string{}, Repo: repo, NextNum: 1}
}

// LoadMemory loads (or creates) a JSON-backed tracker.
func LoadMemory(path, repo string) (*Memory, error) {
	m := NewMemory(repo)
	m.path = path
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, m.save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.LabelsM == nil {
		m.LabelsM = map[string]Label{}
	}
	if m.Comments == nil {
		m.Comments = map[int][]string{}
	}
	return m, nil
}

func (m *Memory) save() error {
	if m.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func (m *Memory) url(kind string, n int) string {
	return fmt.Sprintf("https://github.com/%s/%s/%d", m.Repo, kind, n)
}

// CreateIssue adds an open issue and returns its number.
func (m *Memory) CreateIssue(title, body string, labels ...string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.NextNum
	m.NextNum++
	m.IssuesM[n] = &Issue{Number: n, Title: title, Body: body, URL: m.url("issues", n), State: "open", Labels: append([]string{}, labels...)}
	_ = m.save() // persistence is best-effort; the in-memory state is authoritative
	return n
}

// OpenPR opens a PR and returns its number.
func (m *Memory) OpenPR(author, branch, title, body string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.NextNum
	m.NextNum++
	m.PRsM[n] = &PR{Number: n, Title: title, Body: body, URL: m.url("pull", n), Author: author, State: "open", HeadRef: branch}
	_ = m.save() // persistence is best-effort; the in-memory state is authoritative
	return n
}

// ReviewPR records a review (and optional comment count) on a PR.
func (m *Memory) ReviewPR(n int, author, state string, comments int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr, ok := m.PRsM[n]
	if !ok {
		return fmt.Errorf("no PR #%d", n)
	}
	pr.Reviewers = append(pr.Reviewers, Review{Author: author, State: state})
	for i := 0; i < comments; i++ {
		pr.CommentAuthors = append(pr.CommentAuthors, author)
	}
	return m.save()
}

// MergePR merges a PR and closes the issues it references.
func (m *Memory) MergePR(n int, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr, ok := m.PRsM[n]
	if !ok {
		return fmt.Errorf("no PR #%d", n)
	}
	pr.State, pr.Merged, pr.MergedAt = "closed", true, at
	for _, i := range Closes(*pr) {
		if is, ok := m.IssuesM[i]; ok {
			is.State = "closed"
		}
	}
	return m.save()
}

// SetLabels replaces an issue's labels (used to model a due-date change).
func (m *Memory) SetLabels(n int, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if is, ok := m.IssuesM[n]; ok {
		is.Labels = append([]string{}, labels...)
	}
	_ = m.save() // persistence is best-effort; the in-memory state is authoritative
}

// Issues implements Tracker.
func (m *Memory) Issues() ([]Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Issue
	for _, is := range m.IssuesM {
		if is.State == "open" {
			out = append(out, cloneIssue(*is))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func cloneIssue(is Issue) Issue {
	is.Labels = append([]string{}, is.Labels...)
	is.Assignees = append([]string{}, is.Assignees...)
	return is
}

// Issue implements Tracker.
func (m *Memory) Issue(n int) (Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	is, ok := m.IssuesM[n]
	if !ok {
		return Issue{}, fmt.Errorf("no issue #%d", n)
	}
	return cloneIssue(*is), nil
}

// PRs implements Tracker.
func (m *Memory) PRs() ([]PR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PR
	for _, pr := range m.PRsM {
		out = append(out, *pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// Reviews implements Tracker.
func (m *Memory) Reviews(n int) ([]Review, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pr, ok := m.PRsM[n]
	if !ok {
		return nil, nil, fmt.Errorf("no PR #%d", n)
	}
	return append([]Review{}, pr.Reviewers...), append([]string{}, pr.CommentAuthors...), nil
}

// EnsureLabels implements Tracker.
func (m *Memory) EnsureLabels(ls []Label) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range ls {
		m.LabelsM[l.Name] = l
	}
	return m.save()
}

// AddLabels implements Tracker.
func (m *Memory) AddLabels(n int, labels ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	is, ok := m.IssuesM[n]
	if !ok {
		return fmt.Errorf("no issue #%d", n)
	}
	for _, l := range labels {
		if !HasLabel(*is, l) {
			is.Labels = append(is.Labels, l)
		}
	}
	return m.save()
}

// RemoveLabel implements Tracker.
func (m *Memory) RemoveLabel(n int, label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	is, ok := m.IssuesM[n]
	if !ok {
		return fmt.Errorf("no issue #%d", n)
	}
	kept := is.Labels[:0]
	for _, l := range is.Labels {
		if !strings.EqualFold(l, label) {
			kept = append(kept, l)
		}
	}
	is.Labels = kept
	return m.save()
}

// Assign implements Tracker.
func (m *Memory) Assign(n int, login string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	is, ok := m.IssuesM[n]
	if !ok {
		return fmt.Errorf("no issue #%d", n)
	}
	is.Assignees = append(is.Assignees, login)
	return m.save()
}

// Comment implements Tracker.
func (m *Memory) Comment(n int, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Comments[n] = append(m.Comments[n], body)
	return m.save()
}
