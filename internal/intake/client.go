package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxResponseBytes caps how much of a Switch response body we read.
const maxResponseBytes = 1 << 20

// defaultBackoff is the retry schedule for 5xx / 429 / network errors.
var defaultBackoff = []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second}

// HTTPError is a non-2xx response from the Switch server.
type HTTPError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	return fmt.Sprintf("switch %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, body)
}

func isStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}

func retryable(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status >= 500 || he.Status == http.StatusTooManyRequests
	}
	// Context cancellation is final; anything else (dial, reset, timeout) is retried.
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// Client talks to a Switch server using the agent protocol.
type Client struct {
	Base    string
	AgentID string
	APIKey  string
	HTTP    *http.Client
	Backoff []time.Duration
	Log     *slog.Logger

	mu    sync.Mutex
	rooms map[string]string // room name/id (normalized) -> room id
}

// NewClient builds a client with sane timeouts.
func NewClient(base, agentID, apiKey string) *Client {
	return &Client{
		Base:    strings.TrimRight(base, "/"),
		AgentID: agentID,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
		Backoff: defaultBackoff,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (c *Client) logger() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return c.Log
}

// do performs one JSON request with retries. out may be nil.
func (c *Client) do(ctx context.Context, method, path, token string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		lastErr = c.once(ctx, method, path, token, payload, out)
		if lastErr == nil || !retryable(lastErr) || attempt >= len(c.Backoff) {
			return lastErr
		}
		wait := c.Backoff[attempt]
		c.logger().Warn("switch request failed; retrying", "method", method, "path", path, "attempt", attempt+1, "wait", wait, "err", lastErr)
		select {
		case <-ctx.Done():
			return errors.Join(lastErr, ctx.Err())
		case <-time.After(wait):
		}
	}
}

func (c *Client) once(ctx context.Context, method, path, token string, payload []byte, out any) error {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dibs-intake/1")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: string(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("switch %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// IntegrationProfile describes how the agent integrates with Switch.
type IntegrationProfile struct {
	ConnectionModel         string   `json:"connection_model"`
	MessageExchange         bool     `json:"message_exchange"`
	PreInvocationMediation  []string `json:"pre_invocation_mediation"`
	PostInvocationMediation []string `json:"post_invocation_mediation"`
	EventReporting          []string `json:"event_reporting"`
}

// RegisterRequest is the POST /agents body.
type RegisterRequest struct {
	Name               string             `json:"name"`
	DisplayName        string             `json:"display_name,omitempty"`
	Description        string             `json:"description"`
	ConnectorType      string             `json:"connector_type"`
	IntegrationProfile IntegrationProfile `json:"integration_profile"`
	Overwrite          bool               `json:"overwrite,omitempty"`
}

// RegisterResponse is the POST /agents response.
type RegisterResponse struct {
	ID     string `json:"id"`
	APIKey string `json:"api_key"`
}

// Register creates the agent using a registration key.
func (c *Client) Register(ctx context.Context, registrationKey string, req RegisterRequest) (RegisterResponse, error) {
	var resp RegisterResponse
	if err := c.do(ctx, http.MethodPost, "/agents", registrationKey, req, &resp); err != nil {
		return resp, err
	}
	if resp.ID == "" || resp.APIKey == "" {
		return resp, errors.New("switch register: response missing id or api_key")
	}
	return resp, nil
}

// Health checks GET /health (public).
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", "", nil, nil)
}

// SendMessage posts content to a room by id.
func (c *Client) SendMessage(ctx context.Context, roomID, content string) error {
	body := map[string]string{"room_id": roomID, "content": content}
	return c.do(ctx, http.MethodPost, "/agents/"+c.AgentID+"/message", c.APIKey, body, nil)
}

// Op calls an agent operation and returns the raw "result".
func (c *Client) Op(ctx context.Context, op string, args any) (json.RawMessage, error) {
	if args == nil {
		args = map[string]any{}
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/agents/"+c.AgentID+"/ops/"+op, c.APIKey, args, &resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// Room is a room the agent belongs to.
type Room struct {
	ID   string
	Name string
}

// ListRooms calls the list_rooms operation and parses the result defensively.
func (c *Client) ListRooms(ctx context.Context) ([]Room, error) {
	raw, err := c.Op(ctx, "list_rooms", nil)
	if err != nil {
		return nil, err
	}
	return parseRooms(raw)
}

func parseRooms(raw json.RawMessage) ([]Room, error) {
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		var obj map[string]json.RawMessage
		if err2 := json.Unmarshal(raw, &obj); err2 != nil {
			return nil, fmt.Errorf("list_rooms: unexpected result: %s", truncate(string(raw), 200))
		}
		found := false
		for _, k := range []string{"rooms", "items", "data", "results"} {
			if v, ok := obj[k]; ok {
				if err := json.Unmarshal(v, &items); err == nil {
					found = true
					break
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("list_rooms: unexpected result: %s", truncate(string(raw), 200))
		}
	}
	rooms := make([]Room, 0, len(items))
	for _, it := range items {
		r := Room{ID: firstString(it, "id", "room_id"), Name: firstString(it, "name", "display_name", "title")}
		if r.ID != "" {
			rooms = append(rooms, r)
		}
	}
	return rooms, nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func normRoom(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "#"))
}

// ResolveRoom maps a room name (or id) to a room id, caching the answer.
func (c *Client) ResolveRoom(ctx context.Context, name string) (string, error) {
	key := normRoom(name)
	c.mu.Lock()
	if id, ok := c.rooms[key]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	rooms, err := c.ListRooms(ctx)
	if err != nil {
		return "", err
	}
	var id string
	for _, r := range rooms {
		if r.ID == strings.TrimSpace(name) {
			id = r.ID
			break
		}
	}
	if id == "" {
		for _, r := range rooms {
			if normRoom(r.Name) == key {
				id = r.ID
				break
			}
		}
	}
	if id == "" {
		names := make([]string, 0, len(rooms))
		for _, r := range rooms {
			names = append(names, r.Name)
		}
		sort.Strings(names)
		return "", fmt.Errorf("room %q not found among rooms this agent is in %v; invite it with `!invite-agent @<agent-name>` in that room", name, names)
	}
	c.mu.Lock()
	if c.rooms == nil {
		c.rooms = map[string]string{}
	}
	c.rooms[key] = id
	c.mu.Unlock()
	return id, nil
}

// ForgetRoom drops a cached room id.
func (c *Client) ForgetRoom(name string) {
	c.mu.Lock()
	delete(c.rooms, normRoom(name))
	c.mu.Unlock()
}

// Post sends content to a room by name, re-resolving the room once on 404.
func (c *Client) Post(ctx context.Context, roomName, content string) error {
	id, err := c.ResolveRoom(ctx, roomName)
	if err != nil {
		return err
	}
	err = c.SendMessage(ctx, id, content)
	if err == nil || !isStatus(err, http.StatusNotFound) {
		return err
	}
	c.logger().Warn("room id rejected; refreshing room cache", "room", roomName, "room_id", id)
	c.ForgetRoom(roomName)
	if id, err = c.ResolveRoom(ctx, roomName); err != nil {
		return err
	}
	return c.SendMessage(ctx, id, content)
}
