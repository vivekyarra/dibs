package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vivekyarra/dibs/internal/config"
	"github.com/vivekyarra/dibs/internal/tracker"
)

// Exit codes of Main.
const (
	ExitPass  = 0
	ExitFail  = 1
	ExitSetup = 2
)

// newGitHub is swapped in tests.
var newGitHub = func(repo, token string) tracker.Tracker { return tracker.NewGitHub(repo, token) }

const usage = `usage: dibs guard [--config-dir DIR] [--pr-json FILE] [--repo OWNER/NAME] [--tracker-file FILE]

Fails (exit 1) when a pull request closes an issue reserved for a junior's
learning rep and the PR author is not that junior. Exit 0 = pass, 2 = setup error.

In GitHub Actions it reads the pull_request event from $GITHUB_EVENT_PATH,
the repo from $GITHUB_REPOSITORY and the token from $GH_TOKEN/$GITHUB_TOKEN,
and writes a job summary to $GITHUB_STEP_SUMMARY.

  --config-dir DIR     .dibs folder (default: search upward from the working directory)
  --pr-json FILE       read the PR from FILE (a pull_request event, or a bare PR object)
  --repo OWNER/NAME    repository (default: $GITHUB_REPOSITORY, then team.yaml repo)
  --tracker-file FILE  use an offline JSON tracker (as written by the demo) instead of GitHub
`

// Main runs the guard as a CLI (`dibs guard ...`) and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dibs guard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	configDir := fs.String("config-dir", "", "")
	prJSON := fs.String("pr-json", "", "")
	repo := fs.String("repo", "", "")
	trackerFile := fs.String("tracker-file", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitPass
		}
		return ExitSetup
	}
	if fs.NArg() > 0 {
		return setupErr(stdout, stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}

	// 1. The pull request.
	path := *prJSON
	if path == "" {
		path = os.Getenv("GITHUB_EVENT_PATH")
	}
	if path == "" {
		return setupErr(stdout, stderr, errors.New("no pull request: pass --pr-json FILE or run inside GitHub Actions (GITHUB_EVENT_PATH)"))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return setupErr(stdout, stderr, fmt.Errorf("reading PR event: %w", err))
	}
	pr, ok, err := ParseEvent(raw)
	if err != nil {
		return setupErr(stdout, stderr, fmt.Errorf("parsing %s: %w", filepath.Base(path), err))
	}
	if !ok {
		fmt.Fprintln(stdout, "dibs-guard: not a pull_request event; nothing to check.")
		return ExitPass
	}

	// 2. The team config.
	cfg, err := loadConfig(*configDir)
	if err != nil {
		return setupErr(stdout, stderr, err)
	}

	// 3. The tracker.
	r := firstNonEmpty(*repo, os.Getenv("GITHUB_REPOSITORY"), cfg.Team.Repo)
	var tr tracker.Tracker
	if *trackerFile != "" {
		if _, err := os.Stat(*trackerFile); err != nil {
			return setupErr(stdout, stderr, fmt.Errorf("tracker file: %w", err))
		}
		m, err := tracker.LoadMemory(*trackerFile, r)
		if err != nil {
			return setupErr(stdout, stderr, err)
		}
		tr = m
	} else {
		tok, err := tracker.Token()
		if err != nil {
			return setupErr(stdout, stderr, err)
		}
		tr = newGitHub(r, tok)
	}

	// 4. Decide.
	v, err := Evaluate(cfg, tr, pr)
	if err != nil {
		return setupErr(stdout, stderr, err)
	}
	summary := Summary(pr, v)
	fmt.Fprint(stdout, summary)
	if sp := os.Getenv("GITHUB_STEP_SUMMARY"); sp != "" {
		if err := appendFile(sp, summary); err != nil {
			fmt.Fprintf(stderr, "dibs-guard: could not write job summary: %v\n", err)
		}
	}
	if !v.Pass {
		for _, reason := range v.Reasons {
			fmt.Fprintf(stdout, "::error title=dibs-guard::%s\n", escapeData(reason))
		}
		return ExitFail
	}
	return ExitPass
}

// prJSON covers both the GitHub webhook shape (user.login, head.ref, html_url)
// and Dibs' own tracker.PR shape (author, head_ref, url).
type prJSON struct {
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    *string `json:"body"`
	HTMLURL string  `json:"html_url"`
	URL     string  `json:"url"`
	State   string  `json:"state"`
	Author  string  `json:"author"`
	HeadRef string  `json:"head_ref"`
	User    *struct {
		Login string `json:"login"`
	} `json:"user"`
	Head *struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// ParseEvent reads a pull_request (or pull_request_target) event payload, or a
// bare PR object. ok is false when the payload carries no pull request.
func ParseEvent(raw []byte) (pr tracker.PR, ok bool, err error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return pr, false, err
	}
	body := raw
	if p, has := top["pull_request"]; has {
		if bytes.Equal(bytes.TrimSpace(p), []byte("null")) {
			return pr, false, nil
		}
		body = p
	} else if _, hasUser := top["user"]; !hasUser {
		if _, hasAuthor := top["author"]; !hasAuthor {
			return pr, false, nil
		}
	}
	var pj prJSON
	if err := json.Unmarshal(body, &pj); err != nil {
		return pr, false, err
	}
	pr = tracker.PR{Number: pj.Number, Title: pj.Title, State: pj.State, Author: pj.Author, HeadRef: pj.HeadRef}
	if pr.Number == 0 {
		var n struct {
			Number int `json:"number"`
		}
		_ = json.Unmarshal(raw, &n)
		pr.Number = n.Number
	}
	if pj.Body != nil {
		pr.Body = *pj.Body
	}
	pr.URL = firstNonEmpty(pj.HTMLURL, pj.URL)
	if pj.User != nil && pj.User.Login != "" {
		pr.Author = pj.User.Login
	}
	if pj.Head != nil && pj.Head.Ref != "" {
		pr.HeadRef = pj.Head.Ref
	}
	if pr.Author == "" {
		return pr, false, errors.New("pull request has no author (user.login)")
	}
	return pr, true, nil
}

// loadConfig resolves --config-dir: a .dibs folder itself, a folder containing
// .dibs, or (when empty or missing) a search upward from the working directory.
func loadConfig(dir string) (*config.Config, error) {
	if dir != "" {
		if isFile(filepath.Join(dir, "team.yaml")) {
			return config.Load(dir)
		}
		if isFile(filepath.Join(dir, ".dibs", "team.yaml")) {
			return config.Load(filepath.Join(dir, ".dibs"))
		}
		return nil, fmt.Errorf("--config-dir %s: no team.yaml there (expected a .dibs folder)", dir)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	found, err := config.Find(cwd)
	if err != nil {
		return nil, err
	}
	return config.Load(found)
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, s); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func setupErr(stdout, stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "dibs-guard: %v\n", err)
	fmt.Fprintf(stdout, "::error title=dibs-guard setup::%s\n", escapeData(err.Error()))
	return ExitSetup
}

// escapeData escapes a workflow-command message.
func escapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
