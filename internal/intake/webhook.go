package intake

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// MaxBodyBytes caps webhook payload size.
const MaxBodyBytes = 1 << 20

// VerifySignature checks a GitHub X-Hub-Signature-256 header in constant time.
func VerifySignature(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign returns the X-Hub-Signature-256 value for body.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// deliverySet remembers recent delivery ids (bounded FIFO).
type deliverySet struct {
	mu    sync.Mutex
	max   int
	seen  map[string]struct{}
	order []string
}

func newDeliverySet(limit int) *deliverySet {
	return &deliverySet{max: limit, seen: map[string]struct{}{}}
}

// add returns false if id was already seen.
func (d *deliverySet) add(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[id]; ok {
		return false
	}
	d.seen[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > d.max {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return true
}

// linkRe matches GitHub closing keywords ("closes #3", "Fixes: #4", "resolved #5").
var linkRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#(\d+)\b`)

func linkedIssues(texts ...string) []int {
	seen := map[int]bool{}
	var out []int
	for _, t := range texts {
		for _, m := range linkRe.FindAllStringSubmatch(t, -1) {
			n, err := strconv.Atoi(m[1])
			if err == nil && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

type ghPayload struct {
	Action string `json:"action"`
	Issue  *struct {
		Number int `json:"number"`
	} `json:"issue"`
	PullRequest *struct {
		Number int    `json:"number"`
		Merged bool   `json:"merged"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	} `json:"pull_request"`
	Review *struct {
		State string `json:"state"`
	} `json:"review"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

var (
	issueActions = map[string]bool{"opened": true, "reopened": true, "labeled": true, "unlabeled": true, "edited": true, "closed": true}
	prActions    = map[string]bool{"opened": true, "reopened": true, "closed": true, "synchronize": true, "edited": true}
)

// toEvent normalizes a payload; ok=false means "ignore".
func toEvent(event string, p *ghPayload) (Event, bool) {
	switch event {
	case "issues":
		if p.Issue == nil || !issueActions[p.Action] {
			return Event{}, false
		}
		return Event{Kind: "issue", Number: p.Issue.Number, Action: p.Action, Repo: p.Repository.FullName}, true
	case "pull_request":
		if p.PullRequest == nil || !prActions[p.Action] {
			return Event{}, false
		}
		action := p.Action
		switch {
		case action == "closed" && p.PullRequest.Merged:
			action = "merged"
		case action == "synchronize" || action == "edited":
			action = "updated"
		}
		return Event{Kind: "pr", Number: p.PullRequest.Number, Action: action,
			Linked: linkedIssues(p.PullRequest.Title, p.PullRequest.Body), Repo: p.Repository.FullName}, true
	case "pull_request_review":
		if p.PullRequest == nil || p.Action != "submitted" {
			return Event{}, false
		}
		action := "reviewed"
		if p.Review != nil {
			switch strings.ToLower(p.Review.State) {
			case "approved":
				action = "approved"
			case "changes_requested":
				action = "changes requested"
			}
		}
		return Event{Kind: "pr", Number: p.PullRequest.Number, Action: action,
			Linked: linkedIssues(p.PullRequest.Title, p.PullRequest.Body), Repo: p.Repository.FullName}, true
	}
	return Event{}, false
}

// WebhookHandler receives GitHub webhooks and feeds the batcher.
type WebhookHandler struct {
	Secret         []byte // empty means signatures are not checked (only with --insecure-no-secret)
	Batcher        *Batcher
	Log            *slog.Logger
	IgnoreSenders  map[string]bool // lower-case logins whose events are dropped (e.g. the bot dibs acts as)
	deliveries     *deliverySet
	deliveriesOnce sync.Once
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.deliveriesOnce.Do(func() {
		if h.deliveries == nil {
			h.deliveries = newDeliverySet(4096)
		}
	})
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body"})
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	if len(h.Secret) > 0 && !VerifySignature(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		log.Warn("rejected webhook: bad or missing signature", "event", event, "delivery", delivery, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}
	if delivery != "" && !h.deliveries.add(delivery) {
		log.Info("duplicate delivery ignored", "event", event, "delivery", delivery)
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	if event == "ping" {
		log.Info("ping received", "delivery", delivery)
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	var p ghPayload
	if err := json.Unmarshal(body, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if h.IgnoreSenders[strings.ToLower(p.Sender.Login)] {
		log.Info("event from ignored sender", "event", event, "sender", p.Sender.Login, "delivery", delivery)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	ev, ok := toEvent(event, &p)
	if !ok {
		log.Debug("event ignored", "event", event, "action", p.Action, "delivery", delivery)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if !h.Batcher.Add(ev) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "shutting down"})
		return
	}
	log.Info("event queued", "event", event, "action", ev.Action, "number", ev.Number, "repo", ev.Repo, "delivery", delivery)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}
