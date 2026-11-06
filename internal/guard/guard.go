// Package guard is the hard check behind Dibs: a pull request may not close an
// issue that is reserved as a junior's learning rep unless that junior wrote it.
// It runs as `dibs guard` inside GitHub Actions (see action.yml).
package guard

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/vivekyarra/dibs/internal/config"
	"github.com/vivekyarra/dibs/internal/tracker"
)

// ReleasedLabel lifts a reservation without removing the reserved label.
const ReleasedLabel = "dibs:released"

// Verdict is the outcome of checking one pull request.
type Verdict struct {
	Pass    bool     `json:"pass"`
	Reasons []string `json:"reasons,omitempty"` // why it failed, one line per blocked issue
	Checked []int    `json:"checked"`           // issues the PR closes that were inspected
	Notes   []string `json:"notes,omitempty"`   // informational lines (reserved-but-ok, missing issues)
}

// Evaluate checks every issue the PR says it closes. A PR with no closing
// references passes. An issue reserved for a junior (dibs:reserved:<handle>,
// without dibs:released) may only be closed by that junior's GitHub login.
func Evaluate(cfg *config.Config, tr tracker.Tracker, pr tracker.PR) (Verdict, error) {
	v := Verdict{Pass: true, Checked: []int{}}
	if cfg == nil || tr == nil {
		return v, errors.New("guard: config and tracker are required")
	}
	juniors := map[string]string{}
	for _, j := range cfg.Team.Juniors {
		juniors[strings.ToLower(j.Handle)] = j.GitHub
	}
	author := strings.TrimPrefix(strings.TrimSpace(pr.Author), "@")
	for _, n := range tracker.Closes(pr) {
		is, err := tr.Issue(n)
		if err != nil {
			var api *tracker.APIError
			if errors.As(err, &api) && (api.Status == http.StatusNotFound || api.Status == http.StatusGone) {
				v.Notes = append(v.Notes, fmt.Sprintf("#%d was not found in this repository; skipped.", n))
				continue
			}
			return v, fmt.Errorf("reading issue #%d: %w", n, err)
		}
		v.Checked = append(v.Checked, n)
		h := tracker.ReservedFor(is)
		if h == "" {
			continue
		}
		if tracker.HasLabel(is, ReleasedLabel) {
			v.Notes = append(v.Notes, fmt.Sprintf("#%d was reserved for %s but has been released.", n, h))
			continue
		}
		login, ok := juniors[strings.ToLower(h)]
		if !ok || login == "" {
			v.Pass = false
			v.Reasons = append(v.Reasons, fmt.Sprintf("#%d is reserved for %q, who is not a junior in .dibs/team.yaml, so nobody can close it. Fix team.yaml or ask @dibs to release it first.", n, h))
			continue
		}
		if !strings.EqualFold(author, login) {
			v.Pass = false
			v.Reasons = append(v.Reasons, fmt.Sprintf("#%d is reserved for @%s as a learning rep. Agents coach, they don't solve. Ask @dibs to release it first.", n, login))
			continue
		}
		v.Notes = append(v.Notes, fmt.Sprintf("#%d is reserved for @%s, who authored this PR. Nice rep.", n, login))
	}
	return v, nil
}

// Summary renders the verdict as GitHub-flavoured markdown for the job summary.
func Summary(pr tracker.PR, v Verdict) string {
	var b strings.Builder
	if v.Pass {
		b.WriteString("## ✅ dibs-guard passed\n\n")
	} else {
		b.WriteString("## 🙋 dibs-guard blocked this PR\n\n")
	}
	fmt.Fprintf(&b, "PR #%d by @%s", pr.Number, pr.Author)
	if pr.Title != "" {
		fmt.Fprintf(&b, ": %s", pr.Title)
	}
	b.WriteString("\n\n")
	if len(v.Checked) == 0 {
		b.WriteString("No `Closes/Fixes/Resolves #N` references found, so there is nothing reserved to protect.\n")
	} else {
		ids := make([]string, len(v.Checked))
		for i, n := range v.Checked {
			ids[i] = fmt.Sprintf("#%d", n)
		}
		fmt.Fprintf(&b, "Checked: %s\n", strings.Join(ids, ", "))
	}
	if len(v.Reasons) > 0 {
		b.WriteString("\n### Why\n\n")
		for _, r := range v.Reasons {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	if len(v.Notes) > 0 {
		b.WriteString("\n### Notes\n\n")
		for _, r := range v.Notes {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	return b.String()
}
