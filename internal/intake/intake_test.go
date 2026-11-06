package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testRegKey   = "reg_key_0123456789abcdef"
	testAgentKey = "agent_key_0123456789abcdef"
	testAgentID  = "agt_123"
	testRoomID   = "room_team_1"
	testSecret   = "s3cret"
)

type recorded struct {
	Method, Path, Auth string
	Body               map[string]any
}

// stubSwitch is an httptest Switch server that records requests.
type stubSwitch struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	reqs     []recorded
	messages []map[string]any

	listRoomsResult string       // JSON for "result"
	listRoomsCalls  atomic.Int32 //
	failNext        atomic.Int32 // number of upcoming /message calls that return 503
	notFoundRoom    atomic.Value // room id that returns 404 on /message
	registerStatus  int          //
}

func newStubSwitch(t *testing.T) *stubSwitch {
	f := &stubSwitch{t: t, listRoomsResult: `[{"id":"room_other","name":"practice"},{"id":"room_team_1","name":"team"}]`}
	f.notFoundRoom.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *stubSwitch) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &m); err != nil {
			f.t.Errorf("non-JSON body on %s: %q", r.URL.Path, body)
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), m})
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/health":
		w.Write([]byte(`{"status":"ok"}`))
	case r.Method == http.MethodPost && r.URL.Path == "/agents":
		if r.Header.Get("Authorization") != "Bearer "+testRegKey {
			http.Error(w, `{"error":"bad registration key"}`, http.StatusUnauthorized)
			return
		}
		if f.registerStatus != 0 {
			w.WriteHeader(f.registerStatus)
			return
		}
		w.Write([]byte(`{"id":"` + testAgentID + `","api_key":"` + testAgentKey + `"}`))
	case strings.HasPrefix(r.URL.Path, "/agents/"+testAgentID+"/"):
		if r.Header.Get("Authorization") != "Bearer "+testAgentKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/agents/"+testAgentID+"/")
		switch rest {
		case "ops/list_rooms":
			f.listRoomsCalls.Add(1)
			f.mu.Lock()
			res := f.listRoomsResult
			f.mu.Unlock()
			w.Write([]byte(`{"result":` + res + `}`))
		case "message":
			if f.failNext.Load() > 0 {
				f.failNext.Add(-1)
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if m["room_id"] == f.notFoundRoom.Load().(string) {
				http.Error(w, `{"error":"room not found"}`, http.StatusNotFound)
				return
			}
			f.mu.Lock()
			f.messages = append(f.messages, m)
			f.mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *stubSwitch) msgs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.messages...)
}

func (f *stubSwitch) writeCreds(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds.json")
	if err := SaveCredentials(p, Credentials{Server: f.srv.URL, AgentID: testAgentID, APIKey: testAgentKey, Name: "dibs-intake"}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *stubSwitch) client() *Client {
	c := NewClient(f.srv.URL, testAgentID, testAgentKey)
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	return c
}

func init() {
	clientBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
}

// ---------- signature ----------

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	good := Sign([]byte(testSecret), body)
	cases := []struct {
		name, header string
		want         bool
	}{
		{"valid", good, true},
		{"missing", "", false},
		{"wrong prefix", strings.Replace(good, "sha256=", "sha1=", 1), false},
		{"bad hex", "sha256=zz", false},
		{"wrong secret", Sign([]byte("other"), body), false},
	}
	for _, tc := range cases {
		if got := VerifySignature([]byte(testSecret), body, tc.header); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// ---------- webhook handler ----------

type sink struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (s *sink) send(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.texts = append(s.texts, text)
	return nil
}

func (s *sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

func newTestHandler(secret string, debounce time.Duration) (*WebhookHandler, *Batcher, *sink) {
	s := &sink{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := &Batcher{Alias: "dibs", Debounce: debounce, Send: s.send, Log: quiet}
	h := &WebhookHandler{Secret: []byte(secret), Batcher: b, Log: quiet, IgnoreSenders: map[string]bool{"dibs-bot": true}}
	return h, b, s
}

func post(t *testing.T, h http.Handler, event, delivery, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func signed(t *testing.T, h http.Handler, event, delivery, body string) *httptest.ResponseRecorder {
	return post(t, h, event, delivery, body, Sign([]byte(testSecret), []byte(body)))
}

func issueBody(action string, n int) string {
	return `{"action":"` + action + `","issue":{"number":` + itoa(n) + `},"repository":{"full_name":"o/r"},"sender":{"login":"asha"}}`
}

func prBody(action string, n int, merged bool, body string) string {
	b, _ := json.Marshal(map[string]any{
		"action":       action,
		"pull_request": map[string]any{"number": n, "merged": merged, "title": "t", "body": body},
		"repository":   map[string]any{"full_name": "o/r"},
		"sender":       map[string]any{"login": "asha"},
	})
	return string(b)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestWebhookSignature(t *testing.T) {
	h, b, _ := newTestHandler(testSecret, time.Hour)
	body := issueBody("opened", 4)
	if rr := post(t, h, "issues", "d1", body, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig: %d", rr.Code)
	}
	if rr := post(t, h, "issues", "d2", body, Sign([]byte("nope"), []byte(body))); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: %d", rr.Code)
	}
	if b.Pending() != 0 {
		t.Fatal("unsigned events must not be queued")
	}
	if rr := signed(t, h, "issues", "d3", body); rr.Code != http.StatusAccepted {
		t.Fatalf("valid sig: %d %s", rr.Code, rr.Body)
	}
	if b.Pending() != 1 {
		t.Fatalf("pending=%d", b.Pending())
	}
	// A rejected delivery must not poison dedupe: d1 retried with a valid signature is accepted.
	if rr := signed(t, h, "issues", "d1", body); rr.Code != http.StatusAccepted {
		t.Fatalf("retry after reject: %d", rr.Code)
	}
}

func TestWebhookNoSecretMode(t *testing.T) {
	h, b, _ := newTestHandler("", time.Hour)
	if rr := post(t, h, "issues", "d1", issueBody("opened", 1), ""); rr.Code != http.StatusAccepted {
		t.Fatalf("got %d", rr.Code)
	}
	if b.Pending() != 1 {
		t.Fatal("expected queued")
	}
}

func TestWebhookPingMethodsAndErrors(t *testing.T) {
	h, b, _ := newTestHandler(testSecret, time.Hour)
	if rr := signed(t, h, "ping", "p1", `{"zen":"hi"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "pong") {
		t.Fatalf("ping: %d %s", rr.Code, rr.Body)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rr.Code)
	}
	if rr := signed(t, h, "issues", "bad", `{not json`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rr.Code)
	}
	big := `{"x":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
	if rr := signed(t, h, "issues", "big", big); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big: %d", rr.Code)
	}
	for _, c := range []struct{ event, body string }{
		{"issues", issueBody("assigned", 1)},
		{"issues", `{"action":"opened"}`},
		{"pull_request", prBody("labeled", 2, false, "")},
		{"pull_request_review", prBody("dismissed", 2, false, "")},
		{"push", `{"ref":"main"}`},
		{"issues", `{"action":"opened","issue":{"number":9},"sender":{"login":"Dibs-Bot"}}`},
	} {
		if rr := signed(t, h, c.event, "", c.body); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "ignored") {
			t.Errorf("%s %s: %d %s", c.event, c.body, rr.Code, rr.Body)
		}
	}
	if b.Pending() != 0 {
		t.Fatalf("ignored events queued: %d", b.Pending())
	}
}

func TestWebhookDedupe(t *testing.T) {
	h, b, s := newTestHandler(testSecret, time.Hour)
	body := issueBody("opened", 4)
	if rr := signed(t, h, "issues", "same", body); rr.Code != http.StatusAccepted {
		t.Fatal(rr.Code)
	}
	rr := signed(t, h, "issues", "same", body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "duplicate") {
		t.Fatalf("dup: %d %s", rr.Code, rr.Body)
	}
	if b.Pending() != 1 {
		t.Fatalf("pending=%d", b.Pending())
	}
	_ = b.Flush(context.Background())
	if got := s.all(); len(got) != 1 {
		t.Fatalf("messages: %v", got)
	}
}

func TestDeliverySetBounded(t *testing.T) {
	d := newDeliverySet(2)
	for _, id := range []string{"a", "b", "c"} {
		if !d.add(id) {
			t.Fatal(id)
		}
	}
	if d.add("c") {
		t.Fatal("c should be a dup")
	}
	if !d.add("a") {
		t.Fatal("a should have been evicted")
	}
}

func TestWebhookShuttingDown(t *testing.T) {
	h, b, _ := newTestHandler(testSecret, time.Hour)
	_ = b.Close(context.Background())
	if rr := signed(t, h, "issues", "x", issueBody("opened", 1)); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rr.Code)
	}
}

// ---------- batching / rendering ----------

func TestBatchingIntoOneMessage(t *testing.T) {
	h, b, s := newTestHandler(testSecret, 50*time.Millisecond)
	events := []struct{ ev, body string }{
		{"issues", issueBody("opened", 5)},
		{"issues", issueBody("opened", 4)},
		{"issues", issueBody("labeled", 4)}, // folded into "new"
		{"pull_request", prBody("opened", 12, false, "Closes #3")},
		{"pull_request", prBody("synchronize", 12, false, "Closes #3")},
		{"pull_request", prBody("closed", 12, true, "Closes #3")},
		{"pull_request_review", `{"action":"submitted","review":{"state":"approved"},"pull_request":{"number":14,"body":""},"sender":{"login":"ravi"}}`},
		{"issues", issueBody("closed", 7)},
	}
	for i, e := range events {
		if rr := signed(t, h, e.ev, "d"+itoa(i), e.body); rr.Code != http.StatusAccepted {
			t.Fatalf("%d: %d %s", i, rr.Code, rr.Body)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(s.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := s.all()
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d: %v", len(got), got)
	}
	want := "@dibs new issues: #4 #5 — please `sweep`\nPR activity on #3 (#12 merged), #14 approved; issue updates: #7 closed — please `sync`"
	if got[0] != want {
		t.Fatalf("message:\n got %q\nwant %q", got[0], want)
	}
	if b.Pending() != 0 {
		t.Fatal("pending after flush")
	}
}

func TestRenderVariants(t *testing.T) {
	cases := []struct {
		events []Event
		want   string
	}{
		{nil, ""},
		{[]Event{{Kind: "issue", Number: 4, Action: "opened"}}, "@dibs new issue: #4 — please `sweep`"},
		{[]Event{{Kind: "pr", Number: 9, Action: "opened"}, {Kind: "pr", Number: 9, Action: "updated"}}, "@dibs PR activity: #9 opened — please `sync`"},
		{[]Event{{Kind: "pr", Number: 9, Action: "approved"}, {Kind: "pr", Number: 9, Action: "updated"}}, "@dibs PR activity: #9 updated — please `sync`"},
		{[]Event{{Kind: "issue", Number: 4, Action: "opened"}, {Kind: "issue", Number: 4, Action: "closed"}}, "@dibs new issue: #4 — please `sweep`\nissue updates: #4 closed — please `sync`"},
		{[]Event{{Kind: "issue", Number: 6, Action: "closed"}, {Kind: "issue", Number: 6, Action: "reopened"}}, "@dibs new issue: #6 — please `sweep`"},
		{[]Event{{Kind: "pr", Number: 2, Action: "opened", Linked: []int{1, 3}}}, "@dibs PR activity on #1 (#2 opened), #3 (#2 opened) — please `sync`"},
	}
	for i, c := range cases {
		p := newPending()
		for _, e := range c.events {
			p.add(e)
		}
		if got := p.Render("@dibs"); got != c.want {
			t.Errorf("%d: got %q want %q", i, got, c.want)
		}
	}
}

func TestLinkedIssues(t *testing.T) {
	got := linkedIssues("Fix login", "Closes #3, fixes: #4 and Resolved #3; see #9; prefix#10")
	if len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("got %v", got)
	}
}

func TestBatcherMaxWaitAndFailure(t *testing.T) {
	s := &sink{}
	b := &Batcher{Alias: "dibs", Debounce: 40 * time.Millisecond, MaxWait: 60 * time.Millisecond, Send: s.send}
	start := time.Now()
	// Keep adding events faster than the debounce; MaxWait must still force a flush.
	for time.Since(start) < 150*time.Millisecond && len(s.all()) == 0 {
		b.Add(Event{Kind: "issue", Number: 1, Action: "opened"})
		time.Sleep(10 * time.Millisecond)
	}
	if len(s.all()) == 0 {
		t.Fatal("max wait did not force a flush")
	}
	s.mu.Lock()
	s.err = io.ErrUnexpectedEOF
	s.mu.Unlock()
	b.Add(Event{Kind: "issue", Number: 2, Action: "opened"})
	if err := b.Close(context.Background()); err == nil {
		t.Fatal("expected send error")
	}
	if b.Add(Event{Kind: "issue", Number: 3, Action: "opened"}) {
		t.Fatal("add after close should fail")
	}
}

// ---------- Switch client ----------

func TestRoomResolutionAndCache(t *testing.T) {
	f := newStubSwitch(t)
	c := f.client()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id, err := c.ResolveRoom(ctx, "#Team")
		if err != nil || id != testRoomID {
			t.Fatalf("resolve: %q %v", id, err)
		}
	}
	if n := f.listRoomsCalls.Load(); n != 1 {
		t.Fatalf("list_rooms called %d times, want 1 (cache)", n)
	}
	if id, err := c.ResolveRoom(ctx, "room_other"); err != nil || id != "room_other" {
		t.Fatalf("by id: %q %v", id, err)
	}
	if _, err := c.ResolveRoom(ctx, "missing"); err == nil || !strings.Contains(err.Error(), "invite-agent") {
		t.Fatalf("missing room err: %v", err)
	}
}

func TestParseRoomsShapes(t *testing.T) {
	for _, raw := range []string{
		`[{"id":"r1","name":"team"}]`,
		`{"rooms":[{"room_id":"r1","display_name":"team"}]}`,
		`{"items":[{"id":"r1","title":"team"},{"name":"no-id"}]}`,
	} {
		rooms, err := parseRooms(json.RawMessage(raw))
		if err != nil || len(rooms) != 1 || rooms[0].ID != "r1" || rooms[0].Name != "team" {
			t.Errorf("%s: %+v %v", raw, rooms, err)
		}
	}
	for _, raw := range []string{`"nope"`, `{"other":1}`} {
		if _, err := parseRooms(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: expected error", raw)
		}
	}
	if s := firstString(map[string]any{"id": float64(42)}, "id"); s != "42" {
		t.Errorf("numeric id: %q", s)
	}
}

func TestPostRefreshesRoomOn404(t *testing.T) {
	f := newStubSwitch(t)
	c := f.client()
	ctx := context.Background()
	if err := c.Post(ctx, "team", "hello"); err != nil {
		t.Fatal(err)
	}
	// Room gets a new id; the cached one now 404s.
	f.mu.Lock()
	f.listRoomsResult = `{"rooms":[{"id":"room_team_2","name":"team"}]}`
	f.mu.Unlock()
	f.notFoundRoom.Store(testRoomID)
	if err := c.Post(ctx, "team", "again"); err != nil {
		t.Fatal(err)
	}
	m := f.msgs()
	if len(m) != 2 || m[1]["room_id"] != "room_team_2" || m[1]["content"] != "again" {
		t.Fatalf("messages: %v", m)
	}
	if n := f.listRoomsCalls.Load(); n != 2 {
		t.Fatalf("list_rooms calls %d", n)
	}
}

func TestRetriesOn5xx(t *testing.T) {
	f := newStubSwitch(t)
	c := f.client()
	f.failNext.Store(2)
	if err := c.SendMessage(context.Background(), testRoomID, "x"); err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}
	f.failNext.Store(10)
	err := c.SendMessage(context.Background(), testRoomID, "y")
	if !isStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("expected 503 after exhausting retries, got %v", err)
	}
	f.failNext.Store(0)
	// 4xx is not retried.
	bad := NewClient(f.srv.URL, testAgentID, "wrong")
	bad.Backoff = nil
	if err := bad.SendMessage(context.Background(), testRoomID, "z"); !isStatus(err, http.StatusUnauthorized) {
		t.Fatalf("got %v", err)
	}
	// Network error is retried then surfaced; canceled context stops retries.
	dead := NewClient("http://127.0.0.1:1", testAgentID, testAgentKey)
	dead.Backoff = []time.Duration{time.Millisecond}
	if err := dead.Health(context.Background()); err == nil {
		t.Fatal("expected network error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dead.Health(ctx); err == nil {
		t.Fatal("expected ctx error")
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPErrorTruncates(t *testing.T) {
	e := &HTTPError{Method: "POST", Path: "/x", Status: 500, Body: strings.Repeat("a", 500)}
	if len(e.Error()) > 400 {
		t.Fatal("not truncated")
	}
}

// ---------- register / send / creds ----------

func TestRegisterWritesCreds(t *testing.T) {
	f := newStubSwitch(t)
	out := filepath.Join(t.TempDir(), "sub", "intake.json")
	var stdout, stderr bytes.Buffer
	code := Main([]string{"register", "--server", f.srv.URL + "/", "--registration-key", testRegKey, "--out", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), testAgentKey) {
		t.Fatal("api key printed unmasked")
	}
	if !strings.Contains(stdout.String(), "!invite-agent @dibs-intake") {
		t.Fatalf("stdout: %s", stdout.String())
	}
	creds, err := LoadCredentials(out)
	if err != nil {
		t.Fatal(err)
	}
	if creds != (Credentials{Server: f.srv.URL, AgentID: testAgentID, APIKey: testAgentKey, Name: "dibs-intake"}) {
		t.Fatalf("creds: %+v", creds)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(out)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("perm %v", st.Mode().Perm())
		}
	}
	// Check the registration request.
	f.mu.Lock()
	r := f.reqs[0]
	f.mu.Unlock()
	if r.Method != "POST" || r.Path != "/agents" || r.Auth != "Bearer "+testRegKey {
		t.Fatalf("req: %+v", r)
	}
	if r.Body["name"] != "dibs-intake" || r.Body["connector_type"] != "dibs-intake" {
		t.Fatalf("body: %v", r.Body)
	}
	prof, _ := r.Body["integration_profile"].(map[string]any)
	if prof["connection_model"] != "always_on" || prof["message_exchange"] != true {
		t.Fatalf("profile: %v", prof)
	}
	for _, k := range []string{"pre_invocation_mediation", "post_invocation_mediation", "event_reporting"} {
		if v, ok := prof[k].([]any); !ok || len(v) != 0 {
			t.Fatalf("%s: %v", k, prof[k])
		}
	}
}

func TestRegisterErrors(t *testing.T) {
	f := newStubSwitch(t)
	dir := t.TempDir()
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"register"}, 2},
		{[]string{"register", "--server", f.srv.URL, "--registration-key", testRegKey, "--name", "Bad Name"}, 2},
		{[]string{"register", "--server", f.srv.URL, "--registration-key", testRegKey, "--connection-model", "x"}, 2},
		{[]string{"register", "--bogus"}, 2},
		{[]string{"register", "--server", f.srv.URL, "--registration-key", "wrong", "--out", filepath.Join(dir, "a.json")}, 1},
	}
	for _, c := range cases {
		var o, e bytes.Buffer
		if got := Main(c.args, &o, &e); got != c.code {
			t.Errorf("%v: code %d want %d (%s)", c.args, got, c.code, e.String())
		}
	}
	// Unwritable output path (a file where a directory is expected).
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	var o, e bytes.Buffer
	if got := Main([]string{"register", "--server", f.srv.URL, "--registration-key", testRegKey, "--out", filepath.Join(blocker, "c.json")}, &o, &e); got != 1 {
		t.Fatalf("code %d", got)
	}
	if strings.Contains(e.String(), testAgentKey) {
		t.Fatal("key leaked")
	}
	// Missing fields in response.
	f.registerStatus = http.StatusNoContent
	if got := Main([]string{"register", "--server", f.srv.URL, "--registration-key", testRegKey, "--out", filepath.Join(dir, "b.json")}, &o, &e); got != 1 {
		t.Fatalf("code %d", got)
	}
}

func TestLoadCredentialsErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCredentials(filepath.Join(dir, "none.json")); err == nil {
		t.Fatal("missing")
	}
	p := filepath.Join(dir, "bad.json")
	os.WriteFile(p, []byte("{"), 0o600)
	if _, err := LoadCredentials(p); err == nil {
		t.Fatal("bad json")
	}
	os.WriteFile(p, []byte(`{"server":"x"}`), 0o600)
	if _, err := LoadCredentials(p); err == nil {
		t.Fatal("incomplete")
	}
	if MaskKey("short") != "****" || MaskKey("abcdefghijklmnop") != "abcd…mnop" {
		t.Fatal("mask")
	}
	if !strings.HasSuffix(DefaultCredsPath(), filepath.Join(".dibs", "intake.json")) {
		t.Fatal(DefaultCredsPath())
	}
}

func TestSendCommand(t *testing.T) {
	f := newStubSwitch(t)
	creds := f.writeCreds(t)
	var o, e bytes.Buffer
	if code := Main([]string{"send", "--creds", creds, "--room", "team", "--text", "@dibs report"}, &o, &e); code != 0 {
		t.Fatalf("code %d: %s", code, e.String())
	}
	m := f.msgs()
	if len(m) != 1 || m[0]["room_id"] != testRoomID || m[0]["content"] != "@dibs report" {
		t.Fatalf("messages %v", m)
	}
	f.mu.Lock()
	for _, r := range f.reqs {
		if r.Auth != "Bearer "+testAgentKey {
			t.Errorf("bad auth on %s: %q", r.Path, r.Auth)
		}
	}
	f.mu.Unlock()
	for _, args := range [][]string{
		{"send", "--creds", creds, "--room", "team"},
		{"send", "--nope"},
	} {
		if code := Main(args, &o, &e); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
	if code := Main([]string{"send", "--creds", filepath.Join(t.TempDir(), "x"), "--room", "team", "--text", "x"}, &o, &e); code != 1 {
		t.Errorf("missing creds: %d", code)
	}
	if code := Main([]string{"send", "--creds", creds, "--room", "nowhere", "--text", "x"}, &o, &e); code != 1 {
		t.Errorf("unknown room: %d", code)
	}
}

func TestMainUsage(t *testing.T) {
	var o, e bytes.Buffer
	if Main(nil, &o, &e) != 2 || Main([]string{"wat"}, &o, &e) != 2 || Main([]string{"help"}, &o, &e) != 0 {
		t.Fatal("usage codes")
	}
}

// ---------- serve end-to-end ----------

func runServe(t *testing.T, args []string) (addr string, stop func() (int, string)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	oldNotify, oldListen := notifyContext, onListen
	notifyContext = func(parent context.Context) (context.Context, context.CancelFunc) { return ctx, cancel }
	onListen = func(a string) { addrCh <- a }
	t.Cleanup(func() { notifyContext, onListen = oldNotify, oldListen })

	var stderr syncBuf
	done := make(chan int, 1)
	go func() {
		done <- Main(append([]string{"serve", "--listen", "127.0.0.1:0"}, args...), io.Discard, &stderr)
	}()
	select {
	case addr = <-addrCh:
	case code := <-done:
		t.Fatalf("serve exited early %d: %s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start")
	}
	return addr, func() (int, string) {
		cancel()
		select {
		case code := <-done:
			return code, stderr.String()
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not stop")
			return -1, ""
		}
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestServeEndToEndFlushOnShutdown(t *testing.T) {
	f := newStubSwitch(t)
	creds := f.writeCreds(t)
	addr, stop := runServe(t, []string{"--creds", creds, "--room", "team", "--secret", testSecret, "--debounce", "1h", "--log-format", "json"})

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, _ = http.Post("http://"+addr+"/healthz", "text/plain", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST healthz %d", resp.StatusCode)
	}
	resp.Body.Close()

	for i, n := range []int{8, 9} {
		body := issueBody("opened", n)
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "issues")
		req.Header.Set("X-GitHub-Delivery", "e2e-"+itoa(i))
		req.Header.Set("X-Hub-Signature-256", Sign([]byte(testSecret), []byte(body)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusAccepted {
			t.Fatalf("webhook: %v %v", resp, err)
		}
		resp.Body.Close()
	}
	if len(f.msgs()) != 0 {
		t.Fatal("should still be batching")
	}
	code, logs := stop()
	if code != 0 {
		t.Fatalf("exit %d: %s", code, logs)
	}
	m := f.msgs()
	if len(m) != 1 || m[0]["content"] != "@dibs new issues: #8 #9 — please `sweep`" || m[0]["room_id"] != testRoomID {
		t.Fatalf("messages: %v", m)
	}
	if strings.Contains(logs, testAgentKey) || strings.Contains(logs, testSecret) {
		t.Fatal("secret leaked in logs")
	}
}

func TestServeUnresolvedRoomAndInsecure(t *testing.T) {
	f := newStubSwitch(t)
	creds := f.writeCreds(t)
	addr, stop := runServe(t, []string{"--creds", creds, "--room", "later", "--insecure-no-secret", "--debounce", "1h", "--ignore-senders", "dibs-bot, other"})
	body := issueBody("opened", 1)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("unsigned in insecure mode: %d", resp.StatusCode)
	}
	code, logs := stop()
	if code != 1 { // flush fails: room never resolved
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(logs, "NOT verified") || !strings.Contains(logs, "could not resolve room") {
		t.Fatalf("logs: %s", logs)
	}
}

func TestServeFlagErrors(t *testing.T) {
	f := newStubSwitch(t)
	creds := f.writeCreds(t)
	t.Setenv("DIBS_WEBHOOK_SECRET", "")
	t.Setenv("DIBS_ROOM", "")
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"serve", "--creds", creds}, 2},
		{[]string{"serve", "--creds", creds, "--room", "team"}, 2},
		{[]string{"serve", "--creds", creds, "--room", "team", "--secret", "x", "--debounce", "-1s"}, 2},
		{[]string{"serve", "--bogus"}, 2},
		{[]string{"serve", "--creds", filepath.Join(t.TempDir(), "none"), "--room", "team", "--secret", "x"}, 1},
		{[]string{"serve", "--creds", creds, "--room", "team", "--secret", "x", "--listen", "256.0.0.1:99999"}, 1},
	}
	for _, c := range cases {
		var o, e bytes.Buffer
		if got := Main(c.args, &o, &e); got != c.code {
			t.Errorf("%v: %d want %d: %s", c.args, got, c.code, e.String())
		}
	}
}
