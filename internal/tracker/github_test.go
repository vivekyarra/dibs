package tracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubGH is a scripted GitHub API: handlers keyed by "METHOD /escaped/path".
type stubGH struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	calls    []string // "METHOD path?query"
	bodies   map[string][]string
}

func newStub(t *testing.T) (*stubGH, *GitHub) {
	f := &stubGH{t: t, handlers: map[string]http.HandlerFunc{}, bodies: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.EscapedPath()
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, key+"?"+r.URL.RawQuery)
		f.bodies[key] = append(f.bodies[key], string(b))
		h := f.handlers[key]
		f.mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("%s: Authorization %q", key, got)
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "dibs" ||
			r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Errorf("%s: missing GitHub headers: %v", key, r.Header)
		}
		if len(b) > 0 && r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: body without JSON content type", key)
		}
		if h == nil {
			t.Errorf("unexpected request %s", key)
			http.Error(w, `{"message":"Not Found"}`, 404)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f, &GitHub{Repo: "o/r", Token: "tok", BaseURL: f.srv.URL, Client: f.srv.Client()}
}

func (f *stubGH) on(key string, h http.HandlerFunc) { f.mu.Lock(); f.handlers[key] = h; f.mu.Unlock() }

func (f *stubGH) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func jsonReply(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
}

func TestGitHubIssuesPaginationAndPRFilter(t *testing.T) {
	f, g := newStub(t)
	due := "2026-11-20T08:00:00Z"
	f.on("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			if r.URL.Query().Get("state") != "open" || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			// "prev"/"last" around "next" must not confuse the parser.
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=9>; rel="last", <%s/repos/o/r/issues?page=2>; rel="next"`, f.srv.URL, f.srv.URL))
			jsonReply([]any{
				map[string]any{"number": 1, "title": "one", "body": "b", "html_url": "u1", "state": "open",
					"labels": []any{map[string]any{"name": "bug"}}, "assignees": []any{map[string]any{"login": "asha"}}},
				map[string]any{"number": 2, "title": "a PR", "state": "open", "pull_request": map[string]any{"merged_at": nil}},
			})(w, r)
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=1>; rel="prev", <%s/repos/o/r/issues?page=3>; rel="next"`, f.srv.URL, f.srv.URL))
			jsonReply([]any{map[string]any{"number": 3, "title": "three", "state": "open", "body": nil,
				"milestone": map[string]any{"due_on": due}}})(w, r)
		case "3":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="prev"`, f.srv.URL))
			jsonReply([]any{})(w, r)
		default:
			t.Errorf("page %s", r.URL.Query().Get("page"))
		}
	})
	is, err := g.Issues()
	if err != nil {
		t.Fatal(err)
	}
	if len(is) != 2 || is[0].Number != 1 || is[1].Number != 3 {
		t.Fatalf("issues %+v", is)
	}
	if !reflect.DeepEqual(is[0].Labels, []string{"bug"}) || !reflect.DeepEqual(is[0].Assignees, []string{"asha"}) || is[0].URL != "u1" {
		t.Fatalf("issue 1 %+v", is[0])
	}
	if want, _ := time.Parse(time.RFC3339, due); !is[1].DueOn.Equal(want) || is[1].Labels == nil || is[1].Assignees == nil {
		t.Fatalf("issue 3 %+v", is[1])
	}
	if f.count("GET /repos/o/r/issues") != 3 {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestGitHubRetryOn500ThenSuccess(t *testing.T) {
	f, g := newStub(t)
	n := 0
	f.on("GET /repos/o/r/issues/5", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		jsonReply(map[string]any{"number": 5, "title": "five", "state": "closed"})(w, r)
	})
	is, err := g.Issue(5)
	if err != nil || is.Number != 5 || is.State != "closed" {
		t.Fatalf("%+v %v", is, err)
	}
	if n != 2 {
		t.Fatalf("calls %d", n)
	}
}

func TestGitHubRateLimit403RetryAfter(t *testing.T) {
	f, g := newStub(t)
	n := 0
	f.on("GET /repos/o/r/issues/5", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"message":"secondary rate limit"}`, http.StatusForbidden)
			return
		}
		jsonReply(map[string]any{"number": 5})(w, r)
	})
	start := time.Now()
	if _, err := g.Issue(5); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("calls %d", n)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Retry-After: 0 should not back off long (%v)", time.Since(start))
	}
	// X-RateLimit-Remaining: 0 is also a rate limit.
	m := 0
	f.on("GET /repos/o/r/issues/6", func(w http.ResponseWriter, r *http.Request) {
		m++
		if m == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "0")
			http.Error(w, "limited", http.StatusForbidden)
			return
		}
		jsonReply(map[string]any{"number": 6})(w, r)
	})
	if _, err := g.Issue(6); err != nil || m != 2 {
		t.Fatalf("%v calls=%d", err, m)
	}
}

func TestGitHubPlain403And404NotRetried(t *testing.T) {
	f, g := newStub(t)
	n := 0
	f.on("GET /repos/o/r/issues/7", func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Error(w, `{"message":"Resource not accessible"}`, http.StatusForbidden)
	})
	_, err := g.Issue(7)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 403 || n != 1 || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("err %v calls %d", err, n)
	}
	if !strings.Contains(ae.Path, "/issues/7") || !strings.Contains(ae.Body, "not accessible") {
		t.Fatalf("APIError %+v", ae)
	}
}

func TestGitHubGivesUpAfterRetries(t *testing.T) {
	f, g := newStub(t)
	n := 0
	f.on("GET /repos/o/r/issues/8", func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Retry-After", "0")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	_, err := g.Issue(8)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || n != 4 {
		t.Fatalf("err %v calls %d", err, n)
	}
}

func TestGitHubRemoveLabel(t *testing.T) {
	f, g := newStub(t)
	f.on("DELETE /repos/o/r/issues/3/labels/dibs:reserved:asha", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Label does not exist"}`, 404)
	})
	if err := g.RemoveLabel(3, "dibs:reserved:asha"); err != nil {
		t.Fatalf("404 must be nil: %v", err)
	}
	f.on("DELETE /repos/o/r/issues/3/labels/needs%20review", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("[]")) })
	if err := g.RemoveLabel(3, "needs review"); err != nil {
		t.Fatalf("escaped label: %v", err)
	}
	f.on("DELETE /repos/o/r/issues/4/labels/x", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusGone) })
	if err := g.RemoveLabel(4, "x"); err == nil {
		t.Fatal("non-404 errors must surface")
	}
}

func TestGitHubEnsureLabels(t *testing.T) {
	f, g := newStub(t)
	f.on("POST /repos/o/r/labels", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		f.mu.Lock()
		bs := f.bodies["POST /repos/o/r/labels"]
		f.mu.Unlock()
		json.Unmarshal([]byte(bs[len(bs)-1]), &in)
		if in["name"] == "dibs:proposed" {
			http.Error(w, `{"message":"Validation Failed","errors":[{"code":"already_exists"}]}`, http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(201)
		w.Write([]byte("{}"))
	})
	f.on("PATCH /repos/o/r/labels/dibs:proposed", jsonReply(map[string]any{}))
	err := g.EnsureLabels([]Label{{"dibs:new", "00ff00", "new"}, {"dibs:proposed", "abcdef", "updated desc"}})
	if err != nil {
		t.Fatal(err)
	}
	pb := f.bodies["PATCH /repos/o/r/labels/dibs:proposed"]
	if len(pb) != 1 {
		t.Fatalf("PATCH calls %v", f.calls)
	}
	var patch map[string]string
	json.Unmarshal([]byte(pb[0]), &patch)
	if !reflect.DeepEqual(patch, map[string]string{"color": "abcdef", "description": "updated desc"}) {
		t.Fatalf("patch body %v", patch)
	}
	// Any other failure stops and surfaces.
	f.on("POST /repos/o/r/labels", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad", 400) })
	if err := g.EnsureLabels([]Label{{"a", "b", "c"}}); err == nil {
		t.Fatal("400 must surface")
	}
}

func TestGitHubWrites(t *testing.T) {
	f, g := newStub(t)
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); w.Write([]byte("{}")) }
	f.on("POST /repos/o/r/issues/3/labels", ok)
	f.on("POST /repos/o/r/issues/3/assignees", ok)
	f.on("POST /repos/o/r/issues/3/comments", ok)
	if err := g.AddLabels(3, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := g.Assign(3, "asha"); err != nil {
		t.Fatal(err)
	}
	if err := g.Comment(3, "hello \"world\""); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"POST /repos/o/r/issues/3/labels":    `{"labels":["a","b"]}`,
		"POST /repos/o/r/issues/3/assignees": `{"assignees":["asha"]}`,
		"POST /repos/o/r/issues/3/comments":  `{"body":"hello \"world\""}`,
	}
	for k, v := range want {
		if got := f.bodies[k]; len(got) != 1 || got[0] != v {
			t.Errorf("%s body %v, want %s", k, got, v)
		}
	}
}

func prJSON(n int, state string, closedAgo, updatedAgo time.Duration, merged bool) map[string]any {
	now := time.Now()
	m := map[string]any{"number": n, "title": fmt.Sprint("pr ", n), "body": "Closes #1", "html_url": fmt.Sprint("u", n),
		"state": state, "user": map[string]any{"login": "asha"}, "head": map[string]any{"ref": fmt.Sprint("b", n)},
		"updated_at": now.Add(-updatedAgo).UTC().Format(time.RFC3339)}
	if state == "closed" {
		m["closed_at"] = now.Add(-closedAgo).UTC().Format(time.RFC3339)
		if merged {
			m["merged_at"] = m["closed_at"]
		}
	}
	return m
}

const day = 24 * time.Hour

func TestGitHubPRsStopsAtCutoff(t *testing.T) {
	f, g := newStub(t)
	f.on("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("page") != "" {
			t.Error("must not fetch past the cutoff")
		}
		switch q.Get("state") {
		case "open":
			jsonReply([]any{prJSON(10, "open", 0, time.Hour, false)})(w, r)
		case "closed":
			if q.Get("sort") != "updated" || q.Get("direction") != "desc" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls?state=closed&page=2>; rel="next"`, f.srv.URL))
			jsonReply([]any{
				prJSON(9, "closed", 2*day, 2*day, true),
				prJSON(8, "closed", 3*day, 3*day, false),
				prJSON(7, "closed", 40*day, 40*day, true), // stop here
				prJSON(6, "closed", 41*day, 41*day, true),
			})(w, r)
		default:
			t.Errorf("unexpected state %q", q.Get("state"))
		}
	})
	prs, err := g.PRs()
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 3 || prs[0].Number != 10 || prs[1].Number != 9 || prs[2].Number != 8 {
		t.Fatalf("prs %+v", prs)
	}
	if prs[0].Merged || !prs[1].Merged || prs[1].MergedAt.IsZero() || prs[2].Merged {
		t.Fatalf("merged flags %+v", prs)
	}
	if prs[1].Author != "asha" || prs[1].HeadRef != "b9" || prs[1].URL != "u9" || prs[1].Body != "Closes #1" {
		t.Fatalf("fields %+v", prs[1])
	}
}

// BUG: PRs() lists by updated_at but stops on the first PR whose *closed_at*
// is older than 30 days. A PR closed long ago that got a fresh comment sorts
// to the top and hides every recently merged PR after it, so `dibs sync`
// never records those merges.
func TestGitHubPRsOldClosedButRecentlyUpdatedDoesNotHideNewer(t *testing.T) {
	f, g := newStub(t)
	f.on("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") == "open" { // works whether or not the fix splits open/closed queries
			jsonReply([]any{})(w, r)
			return
		}
		jsonReply([]any{
			prJSON(3, "closed", 60*day, time.Hour, true), // ancient PR, someone commented today
			prJSON(11, "closed", 2*day, 2*day, true),     // merged two days ago
		})(w, r)
	})
	prs, err := g.PRs()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range prs {
		found = found || p.Number == 11
	}
	if !found {
		t.Fatalf("recently merged PR #11 missing: %+v", prs)
	}
}

func TestGitHubReviews(t *testing.T) {
	f, g := newStub(t)
	f.on("GET /repos/o/r/pulls/11/reviews", jsonReply([]any{
		map[string]any{"user": map[string]any{"login": "maya"}, "state": "CHANGES_REQUESTED"},
		map[string]any{"user": map[string]any{"login": "maya"}, "state": "APPROVED"},
	}))
	f.on("GET /repos/o/r/pulls/11/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls/11/comments?page=2>; rel="next"`, f.srv.URL))
			jsonReply([]any{map[string]any{"user": map[string]any{"login": "maya"}}})(w, r)
			return
		}
		jsonReply([]any{map[string]any{"user": map[string]any{"login": "reviewer-bot"}}})(w, r)
	})
	rv, authors, err := g.Reviews(11)
	if err != nil {
		t.Fatal(err)
	}
	want := []Review{{"maya", "CHANGES_REQUESTED"}, {"maya", "APPROVED"}}
	if !reflect.DeepEqual(rv, want) || !reflect.DeepEqual(authors, []string{"maya", "reviewer-bot"}) {
		t.Fatalf("%+v %v", rv, authors)
	}
	f.on("GET /repos/o/r/pulls/12/reviews", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 404) })
	if _, _, err := g.Reviews(12); err == nil {
		t.Fatal("error must surface")
	}
}

func TestGitHubBadJSON(t *testing.T) {
	f, g := newStub(t)
	f.on("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"not":"an array"}`)) })
	if _, err := g.Issues(); err == nil {
		t.Fatal("object instead of array must error")
	}
	f.on("GET /repos/o/r/issues/1", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{`)) })
	if _, err := g.Issue(1); err == nil {
		t.Fatal("truncated JSON must error")
	}
}

func TestNewGitHubAndToken(t *testing.T) {
	t.Setenv("GITHUB_API_URL", "http://ghe.local/api/v3/")
	g := NewGitHub("o/r", "tok")
	if g.BaseURL != "http://ghe.local/api/v3" || g.Repo != "o/r" || g.Token != "tok" || g.Client == nil {
		t.Fatalf("%+v", g)
	}
	t.Setenv("GITHUB_API_URL", "")
	if NewGitHub("o/r", "").BaseURL != "https://api.github.com" {
		t.Fatal("default base")
	}
	t.Setenv("GH_TOKEN", "  from-gh  ")
	t.Setenv("GITHUB_TOKEN", "from-github")
	if tok, err := Token(); err != nil || tok != "from-gh" {
		t.Fatalf("%q %v", tok, err)
	}
	t.Setenv("GH_TOKEN", "")
	if tok, _ := Token(); tok != "from-github" {
		t.Fatalf("%q", tok)
	}
}

func TestGitHubNoTokenNoAuthHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{"number":1}`))
	}))
	defer srv.Close()
	g := &GitHub{Repo: "o/r", BaseURL: srv.URL, Client: srv.Client()}
	if _, err := g.Issue(1); err != nil || got != "" {
		t.Fatalf("%v auth=%q", err, got)
	}
}
