package tmpl

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"

	"github.com/vivekyarra/dibs"
)

// placeholderRe is Switch's own PLACEHOLDER_RE (core/switch_core/rooms_yaml.py).
var placeholderRe = regexp.MustCompile(`\{(\$?[A-Za-z_][A-Za-z0-9_]*)\}`)

// builtins are placeholders Switch fills without a declaration.
var builtins = map[string]bool{"$creator": true, "$agents_dir": true}

func build(t *testing.T) (string, map[string]any) {
	t.Helper()
	out, err := Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("generated template is not YAML: %v", err)
	}
	return out, doc
}

func strs(node any) []string {
	switch v := node.(type) {
	case string:
		return []string{v}
	case []any:
		var s []string
		for _, e := range v {
			s = append(s, strs(e)...)
		}
		return s
	case map[string]any:
		var s []string
		for k, e := range v {
			s = append(s, k)
			s = append(s, strs(e)...)
		}
		return s
	}
	return nil
}

func list(t *testing.T, doc map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := doc[key].([]any)
	if !ok {
		t.Fatalf("%s: want a list, got %T", key, doc[key])
	}
	var out []map[string]any
	for i, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("%s[%d]: want a mapping, got %T", key, i, e)
		}
		out = append(out, m)
	}
	return out
}

func TestShape(t *testing.T) {
	out, doc := build(t)
	if doc["version"] != 1 {
		t.Errorf("version = %v, want 1", doc["version"])
	}
	if strings.Contains(out, "{{include") || strings.Contains(out, "{{") {
		t.Error("generated template still contains an include marker")
	}
	if _, ok := doc["kickoff"]; ok {
		t.Error("a group template must not have a top-level kickoff")
	}
	known := map[string]bool{"version": true, "params": true, "agents": true, "group": true, "rooms": true, "links": true, "form": true}
	for k := range doc {
		if !known[k] {
			t.Errorf("unexpected top-level key %q", k)
		}
	}

	agents := list(t, doc, "agents")
	if len(agents) != 3 {
		t.Fatalf("agents = %d, want 3", len(agents))
	}
	wantProv := []string{"claude", "claude", "codex"}
	wantName := []string{"{team}-dibs", "{team}-builder", "{team}-reviewer"}
	agentNames := map[string]bool{}
	for i, a := range agents {
		if a["provider"] != wantProv[i] {
			t.Errorf("agent %d provider = %v, want %s", i, a["provider"], wantProv[i])
		}
		if a["name"] != wantName[i] {
			t.Errorf("agent %d name = %v, want %s", i, a["name"], wantName[i])
		}
		agentNames[a["name"].(string)] = true
		for _, f := range []string{"location", "directory", "display_name", "description", "addressing"} {
			if s, _ := a[f].(string); s == "" {
				t.Errorf("agent %v: %s is empty", a["name"], f)
			}
		}
		instr, _ := a["instructions"].(string)
		if len(strings.TrimSpace(instr)) < 200 {
			t.Errorf("agent %v: instructions empty or too short (%d chars)", a["name"], len(instr))
		}
	}

	if g, ok := doc["group"].(map[string]any); !ok || g["name"] != "dibs-{team}" {
		t.Errorf("group = %v, want name dibs-{team}", doc["group"])
	}

	rooms := list(t, doc, "rooms")
	if len(rooms) != 2 {
		t.Fatalf("rooms = %d, want 2", len(rooms))
	}
	roomNames := map[string]bool{}
	wantType := []string{"channel_public", "channel_private"}
	for i, r := range rooms {
		name, _ := r["name"].(string)
		roomNames[name] = true
		if r["channel_type"] != wantType[i] {
			t.Errorf("room %s channel_type = %v, want %s", name, r["channel_type"], wantType[i])
		}
		instr, _ := r["instructions"].(string)
		if !strings.HasPrefix(instr, "DIBS PROTOCOL") {
			t.Errorf("room %s: instructions must be the DIBS PROTOCOL", name)
		}
		k, _ := r["kickoff"].(string)
		if !strings.Contains(k, "@{team}-dibs") {
			t.Errorf("room %s: kickoff must mention the dibs agent", name)
		}
		for _, a := range r["agents"].([]any) {
			if !agentNames[a.(string)] {
				t.Errorf("room %s lists unknown agent %v", name, a)
			}
		}
		aliases, _ := r["aliases"].(map[string]any)
		want := map[string]string{"{team}-dibs": "dibs", "{team}-builder": "builder", "{team}-reviewer": "reviewer"}
		for ag, al := range want {
			if aliases[ag] != al {
				t.Errorf("room %s alias for %s = %v, want %s", name, ag, aliases[ag], al)
			}
		}
		docs, _ := r["docs"].([]any)
		found := false
		for _, d := range docs {
			dm := d.(map[string]any)
			if dm["name"] == "dibs-protocol" && strings.HasPrefix(dm["content"].(string), "DIBS PROTOCOL") {
				found = true
			}
		}
		if !found {
			t.Errorf("room %s: missing dibs-protocol document", name)
		}
	}
	if users := strs(rooms[0]["users"]); strings.Join(users, ",") != "{$creator},{junior}" {
		t.Errorf("team room users = %v", users)
	}
	if users := strs(rooms[1]["users"]); strings.Join(users, ",") != "{junior}" {
		t.Errorf("practice room users = %v (the senior must not be in it)", users)
	}

	links := list(t, doc, "links")
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	l := links[0]
	if l["from"] != "{team}-team" || l["to"] != "{junior_handle}-practice" || l["label"] != "practice" {
		t.Errorf("link = %v", l)
	}
	if !roomNames[l["from"].(string)] || !roomNames[l["to"].(string)] {
		t.Errorf("link refers to a room the template does not create: %v", l)
	}
}

// TestPlaceholders mirrors Switch's template lint: every placeholder used is
// declared or a builtin, and every declared param is used.
func TestPlaceholders(t *testing.T) {
	_, doc := build(t)
	params, ok := doc["params"].(map[string]any)
	if !ok {
		t.Fatal("params missing")
	}
	used := map[string]bool{}
	for k, v := range doc {
		if k == "params" {
			continue
		}
		for _, s := range strs(v) {
			for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
				used[m[1]] = true
			}
		}
	}
	var bad []string
	for name := range used {
		if _, declared := params[name]; !declared && !builtins[name] && name != "agent" {
			bad = append(bad, name)
		}
	}
	// {agent} is only meaningful inside an agents: entry.
	for k, v := range doc {
		if k == "params" || k == "agents" {
			continue
		}
		for _, s := range strs(v) {
			if strings.Contains(s, "{agent}") {
				bad = append(bad, "agent (outside an agents entry)")
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("undeclared placeholders: %v (write <N> style in instructions, not single-word braces)", bad)
	}
	for name := range params {
		if !used[name] {
			t.Errorf("param %q is declared but never used", name)
		}
	}
}

// TestCLICommands keeps the agent instructions on the final dibs CLI.
func TestCLICommands(t *testing.T) {
	cli := map[string]bool{}
	for _, c := range strings.Fields("init reset doctor sync sweep suggest triage score approve decline start status why hint attempt keep-going unlock review release record map report mirror budget skills team demo") {
		cli[c] = true
	}
	cmdRe := regexp.MustCompile("`dibs ([a-z-]+)")
	files, err := fs.Glob(dibs.Assets, "agents/*.md")
	if err != nil || len(files) != 4 {
		t.Fatalf("agents/*.md = %v (%v), want 4 files", files, err)
	}
	for _, f := range files {
		b, _ := fs.ReadFile(dibs.Assets, f)
		for _, m := range cmdRe.FindAllStringSubmatch(string(b), -1) {
			if !cli[m[1]] {
				t.Errorf("%s uses unknown command `dibs %s`", f, m[1])
			}
		}
		for _, old := range []string{"hint log", "--ticket", "dibs propose"} {
			if strings.Contains(string(b), old) {
				t.Errorf("%s uses retired CLI syntax %q", f, old)
			}
		}
	}
}

func TestIncludeIndentation(t *testing.T) {
	fsys := fstest.MapFS{
		SourcePath:    {Data: []byte("a:\n  b: |\n    {{include x.md}}\nc: 1\n")},
		"x.md":        {Data: []byte("one\r\n\r\n  two\r\n")},
		"agents/y.md": {Data: []byte("")},
	}
	got, err := BuildFS(fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := header + "a:\n  b: |\n    one\n\n      two\nc: 1\n"
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	var v map[string]any
	if err := yaml.Unmarshal([]byte(got), &v); err != nil {
		t.Fatal(err)
	}
	if b := v["a"].(map[string]any)["b"]; b != "one\n\n  two\n" {
		t.Errorf("b = %q", b)
	}

	for name, src := range map[string]string{
		"missing file": "a: |\n  {{include nope.md}}\n",
		"inline":       "a: {{include x.md}} tail\n",
		"empty file":   "a: |\n  {{include agents/y.md}}\n",
	} {
		fsys[SourcePath] = &fstest.MapFile{Data: []byte(src)}
		if _, err := BuildFS(fsys); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// TestCommittedTemplateIsFresh fails when switch/dibs.template.yaml was not
// regenerated after editing its source or agents/*.md.
func TestCommittedTemplateIsFresh(t *testing.T) {
	if err := Check(filepath.Join("..", "..", OutputPath)); err != nil {
		t.Fatal(err)
	}
}
