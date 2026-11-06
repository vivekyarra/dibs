package intake

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event is a normalized GitHub event the batcher understands.
type Event struct {
	Kind   string // "issue" or "pr"
	Number int
	Action string // normalized: opened, reopened, closed, merged, labeled, unlabeled, edited, updated, approved, changes requested, reviewed
	Linked []int  // for PRs: issues referenced via "closes #N" etc.
	Repo   string
}

// pending holds everything seen in one batch window.
type pending struct {
	newIssues map[int]bool
	issueUpd  map[int]string
	prs       map[int]*prState
	count     int
}

type prState struct {
	action string
	linked map[int]bool
}

func newPending() *pending {
	return &pending{newIssues: map[int]bool{}, issueUpd: map[int]string{}, prs: map[int]*prState{}}
}

// sticky actions are not overwritten by later low-signal activity in the same window.
var stickyPR = map[string]bool{"merged": true, "closed": true, "opened": true, "reopened": true}

func (p *pending) add(ev Event) {
	p.count++
	switch ev.Kind {
	case "issue":
		if ev.Action == "opened" || ev.Action == "reopened" {
			p.newIssues[ev.Number] = true
			delete(p.issueUpd, ev.Number)
			return
		}
		if p.newIssues[ev.Number] && ev.Action != "closed" {
			return // still new; sweep will see the latest state
		}
		p.issueUpd[ev.Number] = ev.Action
	case "pr":
		st := p.prs[ev.Number]
		if st == nil {
			st = &prState{linked: map[int]bool{}}
			p.prs[ev.Number] = st
		}
		for _, n := range ev.Linked {
			st.linked[n] = true
		}
		switch {
		case st.action == "":
			st.action = ev.Action
		case ev.Action == "merged" || ev.Action == "closed" || ev.Action == "reopened":
			st.action = ev.Action
		case stickyPR[st.action]:
			// keep the more important action
		default:
			st.action = ev.Action
		}
	}
}

func sortedKeys[V any](m map[int]V) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}

func hashes(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, " ")
}

// Render builds the single room message for this batch, or "" if empty.
func (p *pending) Render(alias string) string {
	var lines []string
	if len(p.newIssues) > 0 {
		label := "new issues"
		if len(p.newIssues) == 1 {
			label = "new issue"
		}
		lines = append(lines, fmt.Sprintf("%s: %s — please `sweep`", label, hashes(sortedKeys(p.newIssues))))
	}
	var syncParts []string
	if len(p.prs) > 0 {
		// Group PRs by the issues they reference so the message reads
		// "PR activity on #3 (#12 merged)".
		byIssue := map[int][]string{}
		var unlinked []string
		for _, pr := range sortedKeys(p.prs) {
			st := p.prs[pr]
			desc := fmt.Sprintf("#%d %s", pr, st.action)
			if len(st.linked) == 0 {
				unlinked = append(unlinked, desc)
				continue
			}
			for _, iss := range sortedKeys(st.linked) {
				byIssue[iss] = append(byIssue[iss], desc)
			}
		}
		var items []string
		for _, iss := range sortedKeys(byIssue) {
			items = append(items, fmt.Sprintf("#%d (%s)", iss, strings.Join(byIssue[iss], ", ")))
		}
		items = append(items, unlinked...)
		if len(byIssue) > 0 {
			syncParts = append(syncParts, "PR activity on "+strings.Join(items, ", "))
		} else {
			syncParts = append(syncParts, "PR activity: "+strings.Join(items, ", "))
		}
	}
	if len(p.issueUpd) > 0 {
		var items []string
		for _, n := range sortedKeys(p.issueUpd) {
			items = append(items, fmt.Sprintf("#%d %s", n, p.issueUpd[n]))
		}
		syncParts = append(syncParts, "issue updates: "+strings.Join(items, ", "))
	}
	if len(syncParts) > 0 {
		lines = append(lines, strings.Join(syncParts, "; ")+" — please `sync`")
	}
	if len(lines) == 0 {
		return ""
	}
	return "@" + strings.TrimPrefix(alias, "@") + " " + strings.Join(lines, "\n")
}

// Batcher collects events and sends one message per window.
// The window starts at the first event, is extended by later events
// (trailing debounce), and never exceeds maxWait from the first event.
type Batcher struct {
	Alias    string
	Debounce time.Duration
	MaxWait  time.Duration
	Send     func(ctx context.Context, text string) error
	Log      *slog.Logger

	mu      sync.Mutex
	cur     *pending
	first   time.Time
	timer   *time.Timer
	closed  bool
	sendMu  sync.Mutex
	flushes int
}

// Add queues an event; returns false if the batcher is closed.
func (b *Batcher) Add(ev Event) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	now := time.Now()
	if b.cur == nil {
		b.cur = newPending()
		b.first = now
	}
	b.cur.add(ev)
	wait := b.Debounce
	maxWait := b.MaxWait
	if maxWait <= 0 {
		maxWait = 3 * b.Debounce
	}
	if deadline := b.first.Add(maxWait); now.Add(wait).After(deadline) {
		wait = deadline.Sub(now)
	}
	if wait < 0 {
		wait = 0
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(wait, func() { b.Flush(context.Background()) })
	return true
}

// Pending reports the number of queued events.
func (b *Batcher) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		return 0
	}
	return b.cur.count
}

// Flush sends whatever is queued now.
func (b *Batcher) Flush(ctx context.Context) error {
	// sendMu keeps messages in order and makes Close wait for in-flight sends.
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	b.mu.Lock()
	p := b.cur
	b.cur = nil
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()
	if p == nil {
		return nil
	}
	text := p.Render(b.Alias)
	if text == "" {
		return nil
	}
	log := b.Log
	if log == nil {
		log = slog.Default()
	}
	if err := b.Send(ctx, text); err != nil {
		log.Error("failed to post batch to room", "events", p.count, "err", err)
		return err
	}
	b.mu.Lock()
	b.flushes++
	b.mu.Unlock()
	log.Info("posted batch to room", "events", p.count, "text", text)
	return nil
}

// Close stops accepting events and flushes the pending batch.
func (b *Batcher) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return b.Flush(ctx)
}
