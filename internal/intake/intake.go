// Package intake implements dibs-intake: a custom Switch agent (agent
// protocol) that relays GitHub webhooks into the team room as a single
// batched message addressed to @dibs ("new issues: #4 #5 — please `sweep`").
//
// Subcommands (wired as `dibs intake ...`):
//
//	register  POST /agents with a registration key; saves credentials (0600)
//	serve     HTTP server: POST /webhook (GitHub), GET /healthz
//	send      post one message to a room (e.g. weekly "@dibs report" from cron)
package intake

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Test hooks.
var (
	notifyContext = func(parent context.Context) (context.Context, context.CancelFunc) {
		return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	}
	onListen      = func(addr string) {}
	clientBackoff = defaultBackoff
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

const usage = `usage: dibs intake <command> [flags]

commands:
  register  --server URL --registration-key KEY [--name dibs-intake] [--out FILE]
  serve     --listen :8787 --room ROOM [--dibs-alias dibs] [--secret S] [--debounce 10s] [--creds FILE]
  send      --room ROOM --text TEXT [--creds FILE]

run "dibs intake <command> -h" for details.
`

// Main is the entry point for `dibs intake ...`.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "register":
		return cmdRegister(args[1:], stdout, stderr)
	case "serve":
		return cmdServe(args[1:], stdout, stderr)
	case "send":
		return cmdSend(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "dibs intake: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newLogger(w io.Writer, format string) *slog.Logger {
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, nil))
	}
	return slog.New(slog.NewTextHandler(w, nil))
}

func cmdRegister(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dibs intake register", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", envOr("SWITCH_SERVER", ""), "Switch server base URL (env SWITCH_SERVER)")
	regKey := fs.String("registration-key", envOr("SWITCH_REGISTRATION_KEY", ""), "registration key from the Gateway API Keys page (env SWITCH_REGISTRATION_KEY)")
	name := fs.String("name", "dibs-intake", "agent name (lowercase, [a-z0-9._-])")
	display := fs.String("display-name", "Dibs Intake", "agent display name")
	desc := fs.String("description", "Relays GitHub issue and PR webhooks into the team room so @dibs wakes up.", "agent description")
	connModel := fs.String("connection-model", "always_on", "integration_profile.connection_model (always_on|session_addressable|auto_session|session_passive)")
	msgExchange := fs.Bool("message-exchange", true, "integration_profile.message_exchange")
	overwrite := fs.Bool("overwrite", false, "overwrite an existing agent with the same name")
	out := fs.String("out", DefaultCredsPath(), "where to save credentials (0600 JSON)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *server == "" || *regKey == "" {
		fmt.Fprintln(stderr, "dibs intake register: --server and --registration-key are required")
		return 2
	}
	if !nameRe.MatchString(*name) {
		fmt.Fprintf(stderr, "dibs intake register: invalid --name %q (must match %s)\n", *name, nameRe)
		return 2
	}
	switch *connModel {
	case "always_on", "session_addressable", "auto_session", "session_passive":
	default:
		fmt.Fprintf(stderr, "dibs intake register: invalid --connection-model %q\n", *connModel)
		return 2
	}
	c := NewClient(*server, "", "")
	c.Backoff = clientBackoff
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.Register(ctx, *regKey, RegisterRequest{
		Name:          *name,
		DisplayName:   *display,
		Description:   *desc,
		ConnectorType: "dibs-intake",
		IntegrationProfile: IntegrationProfile{
			ConnectionModel:         *connModel,
			MessageExchange:         *msgExchange,
			PreInvocationMediation:  []string{},
			PostInvocationMediation: []string{},
			EventReporting:          []string{},
		},
		Overwrite: *overwrite,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dibs intake register: %v\n", err)
		return 1
	}
	creds := Credentials{Server: c.Base, AgentID: resp.ID, APIKey: resp.APIKey, Name: *name}
	if err := SaveCredentials(*out, creds); err != nil {
		// The key is shown only once by Switch; without the file it is lost.
		fmt.Fprintf(stderr, "dibs intake register: registered agent %s but could not save credentials: %v\n", resp.ID, err)
		return 1
	}
	fmt.Fprintf(stdout, "registered agent %q id=%s key=%s\ncredentials saved to %s (mode 0600)\nnext: in the team room, run `!invite-agent @%s`\n",
		*name, resp.ID, MaskKey(resp.APIKey), *out, *name)
	return 0
}

func clientFromCreds(path string) (*Client, Credentials, error) {
	creds, err := LoadCredentials(path)
	if err != nil {
		return nil, creds, err
	}
	c := NewClient(creds.Server, creds.AgentID, creds.APIKey)
	c.Backoff = clientBackoff
	return c, creds, nil
}

func cmdSend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dibs intake send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", envOr("DIBS_ROOM", ""), "room name or id (env DIBS_ROOM)")
	text := fs.String("text", "", "message text, e.g. \"@dibs report\"")
	credsPath := fs.String("creds", envOr("DIBS_INTAKE_CREDS", DefaultCredsPath()), "credentials file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *room == "" || strings.TrimSpace(*text) == "" {
		fmt.Fprintln(stderr, "dibs intake send: --room and --text are required")
		return 2
	}
	c, _, err := clientFromCreds(*credsPath)
	if err != nil {
		fmt.Fprintf(stderr, "dibs intake send: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.Post(ctx, *room, *text); err != nil {
		fmt.Fprintf(stderr, "dibs intake send: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "sent to %s\n", *room)
	return 0
}

// NewMux builds the HTTP routes for a running intake server.
func NewMux(h *WebhookHandler, b *Batcher) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/webhook", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "pending": b.Pending()})
	})
	return mux
}

func cmdServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dibs intake serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":8787", "listen address")
	room := fs.String("room", envOr("DIBS_ROOM", ""), "team room name or id (env DIBS_ROOM)")
	alias := fs.String("dibs-alias", "dibs", "agent to address in the room (without @)")
	secret := fs.String("secret", os.Getenv("DIBS_WEBHOOK_SECRET"), "GitHub webhook secret (env DIBS_WEBHOOK_SECRET)")
	insecure := fs.Bool("insecure-no-secret", false, "accept unsigned webhooks (local testing only)")
	debounce := fs.Duration("debounce", 10*time.Second, "batch window; events within it become one message")
	ignore := fs.String("ignore-senders", os.Getenv("DIBS_INTAKE_IGNORE_SENDERS"), "comma-separated GitHub logins whose events are dropped (e.g. the account dibs uses), avoids feedback loops")
	credsPath := fs.String("creds", envOr("DIBS_INTAKE_CREDS", DefaultCredsPath()), "credentials file")
	logFormat := fs.String("log-format", "text", "log format: text|json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *room == "" {
		fmt.Fprintln(stderr, "dibs intake serve: --room is required")
		return 2
	}
	if *secret == "" && !*insecure {
		fmt.Fprintln(stderr, "dibs intake serve: --secret (or DIBS_WEBHOOK_SECRET) is required; pass --insecure-no-secret only for local testing")
		return 2
	}
	if *debounce < 0 {
		fmt.Fprintln(stderr, "dibs intake serve: --debounce must be >= 0")
		return 2
	}
	log := newLogger(stderr, *logFormat)
	c, creds, err := clientFromCreds(*credsPath)
	if err != nil {
		fmt.Fprintf(stderr, "dibs intake serve: %v\n", err)
		return 1
	}
	c.Log = log

	ctx, stop := notifyContext(context.Background())
	defer stop()

	// Resolve the room up front so misconfiguration fails fast; not fatal
	// (the agent may not be invited yet), Post re-resolves later.
	rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
	if id, err := c.ResolveRoom(rctx, *room); err != nil {
		log.Warn("could not resolve room yet; will retry on first message", "room", *room, "err", err)
	} else {
		log.Info("room resolved", "room", *room, "room_id", id)
	}
	rcancel()

	b := &Batcher{
		Alias:    *alias,
		Debounce: *debounce,
		Log:      log,
		Send: func(ctx context.Context, text string) error {
			sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			return c.Post(sctx, *room, text)
		},
	}
	h := &WebhookHandler{Batcher: b, Log: log, IgnoreSenders: map[string]bool{}}
	if *secret != "" {
		h.Secret = []byte(*secret)
	} else {
		log.Warn("webhook signatures are NOT verified (--insecure-no-secret)")
	}
	for _, s := range strings.Split(*ignore, ",") {
		if s = strings.TrimSpace(s); s != "" {
			h.IgnoreSenders[strings.ToLower(s)] = true
		}
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "dibs intake serve: listen: %v\n", err)
		return 1
	}
	srv := &http.Server{
		Handler:           NewMux(h, b),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("dibs-intake listening", "addr", ln.Addr().String(), "agent", creds.Name, "agent_id", creds.AgentID, "room", *room, "debounce", *debounce)
	onListen(ln.Addr().String())

	code := 0
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			code = 1
		}
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	if err := b.Close(shCtx); err != nil {
		code = 1
	}
	log.Info("stopped")
	return code
}
