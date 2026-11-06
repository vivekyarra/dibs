package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInferSkills(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "app/orders/service.py", "app/orders/repo.py", "app/api/routes.py", "migrations/0001.sql",
		"tests/test_orders.py", "node_modules/x/index.js", ".venv/lib/a.py", "main.py", "internal/engine/a.go", "internal/engine/a_test.go")
	got := InferSkills(root)
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	for _, want := range []string{"orders", "api", "migrations", "engine", "testing"} {
		if !names[want] {
			t.Errorf("missing skill %q in %+v", want, got)
		}
	}
	for _, bad := range []string{"node-modules", "lib", "x"} {
		if names[bad] {
			t.Errorf("unexpected skill %q", bad)
		}
	}
	if skillName("pkg/internal") != "pkg" || skillName("Web_UI") != "web-ui" {
		t.Fatal("skill names")
	}
}

func TestScaffold(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "app/orders/service.py", "tests/test_orders.py")
	if _, err := Scaffold(ScaffoldOptions{RepoDir: root, Repo: "acme/shop"}); err == nil || !strings.Contains(err.Error(), "--senior") {
		t.Fatal("needs logins")
	}
	if _, err := Scaffold(ScaffoldOptions{RepoDir: root, Repo: "bad", Senior: "Ravi-GH", Junior: "asha"}); err == nil {
		t.Fatal("bad repo")
	}
	dir, err := Scaffold(ScaffoldOptions{RepoDir: root, Repo: "acme/Shop", Senior: "Ravi-GH", Junior: "asha_dev"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Team.TeamRoom != "shop-team" || c.Team.Seniors[0].Handle != "ravi-gh" || c.Team.Juniors[0].Handle != "asha-dev" || len(c.Skills) < 2 {
		t.Fatalf("%+v", c.Team)
	}
	if _, err := Scaffold(ScaffoldOptions{RepoDir: root, Repo: "acme/shop", Senior: "r", Junior: "j"}); err == nil {
		t.Fatal("must not overwrite without --force")
	}
	if _, err := Scaffold(ScaffoldOptions{RepoDir: root, Repo: "acme/shop", Senior: "r", Junior: "j", Force: true}); err != nil {
		t.Fatal(err)
	}
	// An empty repo still gets a valid config.
	empty := t.TempDir()
	if _, err := Scaffold(ScaffoldOptions{RepoDir: empty, Repo: "acme/empty", Senior: "r", Junior: "j"}); err != nil {
		t.Fatal(err)
	}
}

func TestDetectRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if _, err := DetectRepo(dir); err == nil {
		t.Fatal("no remote")
	}
	for url, want := range map[string]string{
		"https://github.com/acme/shop.git": "acme/shop",
		"git@github.com:acme/shop.git":     "acme/shop",
		"https://github.com/acme/shop":     "acme/shop",
	} {
		d := t.TempDir()
		exec.Command("git", "-C", d, "init", "-q").Run()
		exec.Command("git", "-C", d, "remote", "add", "origin", url).Run()
		if got, err := DetectRepo(d); err != nil || got != want {
			t.Fatalf("%s: %q %v", url, got, err)
		}
	}
	d := t.TempDir()
	exec.Command("git", "-C", d, "init", "-q").Run()
	exec.Command("git", "-C", d, "remote", "add", "origin", "https://gitlab.com/acme/shop.git").Run()
	if _, err := DetectRepo(d); err == nil {
		t.Fatal("non-GitHub remote")
	}
}
