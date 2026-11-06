// Command dibs is the Dibs ledger CLI. Agents in a Switch room call it; every
// command prints JSON whose `messages` the dibs agent posts verbatim.
//
// Exit codes: 0 ok · 1 error · 2 a Dibs rule refused the action.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vivekyarra/dibs/internal/config"
	"github.com/vivekyarra/dibs/internal/guard"
	"github.com/vivekyarra/dibs/internal/intake"
	"github.com/vivekyarra/dibs/internal/ledger"
	"github.com/vivekyarra/dibs/internal/report"
	"github.com/vivekyarra/dibs/internal/service"
	"github.com/vivekyarra/dibs/internal/story"
	"github.com/vivekyarra/dibs/internal/tmpl"
	"github.com/vivekyarra/dibs/internal/tracker"
)

var version = "dev"

const usage = `dibs — your AI teammates call dibs on the right work for your juniors.

Daily flow (run by the dibs agent in your Switch room):
  sweep [--accept]                 sync GitHub, triage new issues, score, post the card
  approve N --by WHO               senior approves a proposed rep   (decline N --by WHO)
  start N --by WHO                 junior starts a rep; posts the brief
  status N                         how an agent may act on a ticket (coach or build)
  hint request N --level L --agent A
  hint give N --level L --agent A --text-file F    check + log a hint before posting it
  attempt N --by WHO --note TEXT   an honest attempt unlocks deeper hints
  review --pr P --verdict changes_requested|ready [--teaching TEXT] --agent A
  release N --by WHO [--reason R]  hand a rep to the builder (never a failure)
  sync                             apply merges, reverts, deadlines, stuck checks
  report [--html FILE]             weekly ramp report (chat) and dashboard

Setup: scaffold --senior LOGIN --junior LOGIN (writes .dibs/), init, doctor

Also: reset, suggest N, triage N, score, why N, keep-going N, unlock N,
      record N, map [WHO], mirror, budget, skills, team, autopilot, demo, template,
      guard, intake, version

Global flags: --config DIR (.dibs folder; default: search upward)  --human (print messages, not JSON)
Exit codes: 0 ok · 1 error · 2 refused by a Dibs rule.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type globals struct {
	configDir string
	human     bool
}

func splitGlobals(args []string) (globals, []string) {
	var g globals
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--human":
			g.human = true
		case a == "--config" && i+1 < len(args):
			g.configDir = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			g.configDir = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
		}
	}
	return g, rest
}

func run(args []string, stdout, stderr io.Writer) int {
	g, args := splitGlobals(args)
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version":
		fmt.Fprintln(stdout, "dibs", version)
		return 0
	case "guard":
		return guard.Main(rest, stdout, stderr)
	case "intake":
		return intake.Main(rest, stdout, stderr)
	case "demo":
		return story.Main(rest, stdout, stderr)
	case "template":
		return templateCmd(rest, stdout, stderr)
	case "scaffold":
		return scaffoldCmd(rest, stdout, stderr)
	}
	svc, closeFn, err := open(g)
	if err != nil {
		return fail(stdout, stderr, g, err)
	}
	defer closeFn()
	res, err := dispatch(svc, cmd, rest)
	if err != nil {
		return fail(stdout, stderr, g, err)
	}
	return emit(stdout, g, res)
}

// open loads config, the ledger and the tracker. DIBS_TRACKER=memory:FILE uses a
// JSON-file tracker (offline demo, tests); otherwise GitHub.
func open(g globals) (*service.Service, func(), error) {
	dir := g.configDir
	if dir == "" {
		d, err := config.Find(".")
		if err != nil {
			return nil, nil, err
		}
		dir = d
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, nil, err
	}
	l, err := ledger.Open(cfg.DB)
	if err != nil {
		return nil, nil, fmt.Errorf("open ledger %s: %w", cfg.DB, err)
	}
	var tr tracker.Tracker
	if spec := os.Getenv("DIBS_TRACKER"); strings.HasPrefix(spec, "memory:") {
		m, err := tracker.LoadMemory(strings.TrimPrefix(spec, "memory:"), cfg.Team.Repo)
		if err != nil {
			l.Close()
			return nil, nil, err
		}
		tr = m
	} else {
		tok, err := tracker.Token()
		if err != nil {
			l.Close()
			return nil, nil, err
		}
		tr = tracker.NewGitHub(cfg.Team.Repo, tok)
	}
	abs, _ := filepath.Abs(dir)
	svc := &service.Service{Cfg: cfg, L: l, T: tr, Now: service.Clock(), RepoDir: filepath.Dir(abs)}
	return svc, func() { l.Close() }, nil
}

func fail(stdout, stderr io.Writer, g globals, err error) int {
	if re, ok := service.IsRule(err); ok {
		if g.human {
			fmt.Fprintln(stderr, "refused:", re.Message)
		} else {
			writeJSON(stdout, map[string]any{"ok": false, "error": re})
		}
		return 2
	}
	if g.human {
		fmt.Fprintln(stderr, "error:", err)
	} else {
		writeJSON(stdout, map[string]any{"ok": false, "error": map[string]string{"code": "error", "message": err.Error()}})
	}
	return 1
}

func emit(stdout io.Writer, g globals, r *service.Result) int {
	if r == nil {
		r = &service.Result{OK: true}
	}
	if r.Messages == nil {
		r.Messages = []service.Message{}
	}
	if !g.human {
		writeJSON(stdout, r)
		return 0
	}
	for _, m := range r.Messages {
		fmt.Fprintf(stdout, "── #%s ──\n%s\n\n", m.Room, m.Text)
	}
	for _, t := range r.AgentTasks {
		fmt.Fprintf(stdout, "• task: %s\n", t)
	}
	if len(r.Messages) == 0 && len(r.AgentTasks) == 0 && r.Data != nil {
		writeJSON(stdout, r.Data)
	}
	return 0
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// flags parses subcommand flags, allowing positional args before or after flags.
type flags struct {
	fs  *flag.FlagSet
	pos []string
}

func newFlags(name string) *flags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &flags{fs: fs}
}

func (f *flags) parse(args []string) error {
	var pos []string
	for len(args) > 0 {
		if err := f.fs.Parse(args); err != nil {
			return &service.RuleError{Code: "usage", Message: err.Error()}
		}
		args = f.fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	f.pos = pos
	return nil
}

func (f *flags) num(i int) (int, error) {
	if i >= len(f.pos) {
		return 0, &service.RuleError{Code: "usage", Message: "missing issue number"}
	}
	return service.ParseNum(f.pos[i])
}

func need(name, v string) error {
	if strings.TrimSpace(v) == "" {
		return &service.RuleError{Code: "usage", Message: "missing --" + name}
	}
	return nil
}

func dispatch(s *service.Service, cmd string, args []string) (*service.Result, error) {
	f := newFlags(cmd)
	by := f.fs.String("by", "", "who is acting (handle, GitHub login or chat name)")
	agent := f.fs.String("agent", "", "agent name")
	level := f.fs.Int("level", 0, "hint level 1-4")
	textFile := f.fs.String("text-file", "", "file with the hint text (- for stdin)")
	text := f.fs.String("text", "", "hint text (prefer --text-file)")
	note := f.fs.String("note", "", "what was tried")
	reason := f.fs.String("reason", "", "reason")
	skills := f.fs.String("skills", "", "skills, e.g. idempotency:p,error-handling:s")
	est := f.fs.Float64("est", 0, "hours for an experienced engineer")
	paths := f.fs.String("paths", "", "comma-separated paths the ticket touches")
	accept := f.fs.Bool("accept", false, "accept Dibs' suggestion")
	pr := f.fs.Int("pr", 0, "pull request number")
	verdict := f.fs.String("verdict", "", "changes_requested or ready")
	teaching := f.fs.String("teaching", "", "two-line teaching moment for the senior")
	slots := f.fs.Int("slots", 0, "reps per junior at a time")
	share := f.fs.Float64("share", 0, "max share of open backlog held as reps")
	html := f.fs.String("html", "", "write the HTML dashboard to this file")
	junior := f.fs.String("junior", "", "only this junior")
	interval := f.fs.Duration("interval", 0, "autopilot poll interval (default 5m)")
	once := f.fs.Bool("once", false, "autopilot: run one cycle and exit")
	dry := f.fs.Bool("dry-run", false, "autopilot: print messages instead of posting to Switch")
	creds := f.fs.String("creds", os.Getenv("DIBS_INTAKE_CREDS"), "autopilot: dibs-intake credentials file")
	if err := f.parse(args); err != nil {
		return nil, err
	}
	actor := *by
	if actor == "" {
		actor = "dibs"
	}
	switch cmd {
	case "init":
		return s.Init()
	case "reset":
		return s.Reset()
	case "doctor":
		return s.Doctor()
	case "sync":
		return s.Sync(actor)
	case "sweep":
		return s.Sweep(actor, *accept)
	case "score":
		return s.Score(actor)
	case "suggest":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		return s.Suggest(n)
	case "triage":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		if *accept {
			sr, err := s.Suggest(n)
			if err != nil {
				return nil, err
			}
			sg := sr.Data.(service.Suggestion)
			return s.Triage(n, sg.Skills, sg.EstSeniorHours, sg.Paths, actor)
		}
		reqs, err := service.ParseSkills(*skills)
		if err != nil {
			return nil, err
		}
		var ps []string
		if *paths != "" {
			ps = strings.Split(*paths, ",")
		}
		return s.Triage(n, reqs, *est, ps, actor)
	case "why":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		return s.Why(n)
	case "approve", "ok":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		if err := need("by", *by); err != nil {
			return nil, err
		}
		return s.Approve(n, *by)
	case "decline", "no":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		if err := need("by", *by); err != nil {
			return nil, err
		}
		return s.Decline(n, *by)
	case "start":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		if err := need("by", *by); err != nil {
			return nil, err
		}
		return s.Start(n, *by)
	case "status", "hints":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		return s.Status(n)
	case "hint":
		if len(f.pos) == 0 {
			return nil, &service.RuleError{Code: "usage", Message: "use `dibs hint request N` or `dibs hint give N`"}
		}
		sub := f.pos[0]
		f.pos = f.pos[1:]
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		name := *agent
		if name == "" {
			name = actor
		}
		switch sub {
		case "request":
			return s.HintRequest(n, *level, name)
		case "give", "log":
			body := *text
			if *textFile != "" {
				var b []byte
				if *textFile == "-" {
					b, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
				} else {
					b, err = os.ReadFile(*textFile)
				}
				if err != nil {
					return nil, err
				}
				body = string(b)
			}
			return s.HintGive(n, *level, name, body)
		}
		return nil, &service.RuleError{Code: "usage", Message: "unknown hint subcommand " + strconv.Quote(sub)}
	case "attempt":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		if err := need("by", *by); err != nil {
			return nil, err
		}
		return s.Attempt(n, *by, *note)
	case "keep-going":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		if err := need("by", *by); err != nil {
			return nil, err
		}
		return s.KeepGoing(n, *by)
	case "unlock":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		if err := need("by", *by); err != nil {
			return nil, err
		}
		lv := *level
		if lv == 0 && len(f.pos) > 1 {
			lv, _ = strconv.Atoi(strings.TrimPrefix(strings.ToUpper(f.pos[1]), "L"))
		}
		return s.Unlock(n, lv, *by)
	case "review":
		p := *pr
		if p == 0 && len(f.pos) > 0 {
			p, _ = service.ParseNum(f.pos[0])
		}
		if p == 0 {
			return nil, &service.RuleError{Code: "usage", Message: "missing --pr"}
		}
		name := *agent
		if name == "" {
			name = actor
		}
		return s.Review(p, *verdict, *teaching, name)
	case "release":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		if err := need("by", *by); err != nil {
			return nil, err
		}
		return s.Release(n, *by, *reason)
	case "record":
		n, err := f.num(0)
		if err != nil {
			return nil, err
		}
		return s.Record(n, actor)
	case "map":
		h := ""
		if len(f.pos) > 0 {
			h = f.pos[0]
		}
		return s.Map(h)
	case "mirror":
		return s.Mirror()
	case "budget":
		if err := need("by", *by); err != nil {
			return nil, err
		}
		return s.Budget(*slots, *share, *by)
	case "skills":
		return s.Skills(), nil
	case "team":
		return s.Team(), nil
	case "report":
		res, err := s.Report(*junior)
		if err != nil {
			return nil, err
		}
		if *html != "" {
			in, ok := res.Data.(report.Input)
			if !ok {
				return nil, errors.New("report input missing")
			}
			b, err := report.HTML(in)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(*html, b, 0o644); err != nil {
				return nil, err
			}
			res.Data = map[string]any{"html": *html}
		} else {
			res.Data = nil
		}
		return res, nil
	case "autopilot":
		opts := service.AutopilotOptions{Interval: *interval, Once: *once, Log: os.Stderr}
		if !*dry {
			path := *creds
			if path == "" {
				path = intake.DefaultCredsPath()
			}
			c, err := intake.LoadCredentials(path)
			if err != nil {
				return nil, &service.RuleError{Code: "no_switch_credentials", Message: fmt.Sprintf("autopilot posts to Switch as the dibs-intake agent; run `dibs intake register` first (%v), or pass --dry-run to print instead", err)}
			}
			opts.Poster = intake.NewClient(c.Server, c.AgentID, c.APIKey)
		}
		return s.Autopilot(opts)
	}
	return nil, &service.RuleError{Code: "usage", Message: fmt.Sprintf("unknown command %q (run `dibs help`)", cmd)}
}

func templateCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("template", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "fail if switch/dibs.template.yaml is out of date")
	out := fs.String("out", "", "write the template to this file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check {
		if err := tmpl.Check(filepath.Join("switch", "dibs.template.yaml")); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "switch/dibs.template.yaml is up to date")
		return 0
	}
	s, err := tmpl.Build()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(s), 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stdout, s)
	return 0
}

func scaffoldCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := config.ScaffoldOptions{}
	fs.StringVar(&o.RepoDir, "dir", ".", "repository root")
	fs.StringVar(&o.Repo, "repo", "", "owner/name (default: from the git remote)")
	fs.StringVar(&o.TeamRoom, "room", "", "Switch team room name (default: <repo>-team)")
	fs.StringVar(&o.Senior, "senior", "", "senior's GitHub login")
	fs.StringVar(&o.Junior, "junior", "", "junior's GitHub login")
	fs.BoolVar(&o.Force, "force", false, "overwrite an existing .dibs folder")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, err := config.Scaffold(o)
	if err != nil {
		fmt.Fprintln(stderr, "scaffold:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s (team.yaml, skills.yaml, dibs.yaml).\nNext: review the skills and starting levels, then run `dibs init` and `dibs doctor`.\n", dir)
	return 0
}
