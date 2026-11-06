// Package tracker is Dibs' view of the issue tracker. GitHub is the real
// implementation; Memory backs the offline demo and the tests.
package tracker

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Issue is an open or closed issue.
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	State     string    `json:"state"` // open | closed
	Labels    []string  `json:"labels"`
	Assignees []string  `json:"assignees"`
	DueOn     time.Time `json:"due_on"` // milestone due date, zero if none
}

// PR is a pull request.
type PR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	URL            string    `json:"url"`
	Author         string    `json:"author"`
	State          string    `json:"state"` // open | closed
	Merged         bool      `json:"merged"`
	MergedAt       time.Time `json:"merged_at"`
	HeadRef        string    `json:"head_ref"`
	Reviewers      []Review  `json:"reviews,omitempty"`
	CommentAuthors []string  `json:"comment_authors,omitempty"` // one entry per review comment
}

// Review is one submitted review.
type Review struct {
	Author string `json:"author"`
	State  string `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED
}

// Label is a label definition.
type Label struct {
	Name, Color, Description string
}

// Tracker is everything Dibs needs from an issue tracker.
type Tracker interface {
	Issues() ([]Issue, error)                   // open issues
	Issue(n int) (Issue, error)                 // any state
	PRs() ([]PR, error)                         // open PRs and PRs closed in the last 30 days
	Reviews(pr int) ([]Review, []string, error) // reviews, and one author per review comment
	EnsureLabels(ls []Label) error
	AddLabels(n int, labels ...string) error
	RemoveLabel(n int, label string) error
	Assign(n int, login string) error
	Comment(n int, body string) error
}

var closesRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+(?:[\w.-]+/[\w.-]+)?#(\d+)`)

// Closes returns the issue numbers a PR body/title says it closes.
func Closes(pr PR) []int {
	seen := map[int]bool{}
	var out []int
	for _, m := range closesRe.FindAllStringSubmatch(pr.Title+"\n"+pr.Body, -1) {
		n, err := strconv.Atoi(m[1])
		if err == nil && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

var revertsRe = regexp.MustCompile(`(?i)\breverts?\s+(?:[\w.-]+/[\w.-]+)?#(\d+)`)

// Reverts returns the PR number this PR reverts, or 0.
func Reverts(pr PR) int {
	if m := revertsRe.FindStringSubmatch(pr.Title + "\n" + pr.Body); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// DueFrom reads a due date from a `due:YYYY-MM-DD` label (end of that day UTC) or a
// `due:YYYY-MM-DDTHH:MM` label (UTC), falling back to the milestone.
func DueFrom(is Issue) time.Time {
	for _, l := range is.Labels {
		if strings.HasPrefix(l, "due:") {
			v := strings.TrimPrefix(l, "due:")
			if d, err := time.Parse("2006-01-02", v); err == nil {
				return d.Add(23*time.Hour + 59*time.Minute)
			}
			for _, layout := range []string{"2006-01-02T15:04", time.RFC3339} {
				if d, err := time.Parse(layout, v); err == nil {
					return d.UTC()
				}
			}
		}
	}
	return is.DueOn
}

// HasLabel reports whether the issue carries a label.
func HasLabel(is Issue, label string) bool {
	for _, l := range is.Labels {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}

// ReservedFor returns the handle in a dibs:reserved:<handle> label, or "".
func ReservedFor(is Issue) string {
	for _, l := range is.Labels {
		if strings.HasPrefix(l, "dibs:reserved:") {
			return strings.TrimPrefix(l, "dibs:reserved:")
		}
	}
	return ""
}
