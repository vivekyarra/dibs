// Package tmpl builds switch/dibs.template.yaml, the Switch template people
// paste into Switch Console, from switch/dibs.template.src.yaml and the
// agent instructions under agents/.
//
// The source marks each inclusion with a line holding only
//
//	{{include agents/<file>.md}}
//
// usually as the first line of a `|` block scalar. Build replaces the line
// with the file, every line indented as the marker was, so the result stays
// valid YAML. Double braces never collide with Switch's `{param}` placeholders.
//
// Regenerate after editing the source or any agents/*.md:
//
//	go generate ./internal/tmpl
package tmpl

//go:generate go run ./gen ../../switch/dibs.template.yaml

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/vivekyarra/dibs"
)

const (
	// SourcePath is the template source inside dibs.Assets.
	SourcePath = "switch/dibs.template.src.yaml"
	// OutputPath is where the generated template is committed, relative to the repo root.
	OutputPath = "switch/dibs.template.yaml"
)

const header = "# GENERATED from switch/dibs.template.src.yaml and agents/*.md; DO NOT EDIT.\n" +
	"# Regenerate with: go generate ./internal/tmpl\n" +
	"# Paste this file into Switch Console: Templates, Import template.\n"

var includeRe = regexp.MustCompile(`^([ \t]*)\{\{include ([^{}\s]+)\}\}[ \t]*$`)

// Build returns the generated template, built from the embedded assets.
func Build() (string, error) {
	return BuildFS(dibs.Assets)
}

// BuildFS builds the template from any file system laid out like the repo.
func BuildFS(fsys fs.FS) (string, error) {
	src, err := fs.ReadFile(fsys, SourcePath)
	if err != nil {
		return "", fmt.Errorf("read template source: %w", err)
	}
	var out strings.Builder
	out.WriteString(header)
	lines := strings.Split(normalize(string(src)), "\n")
	// A trailing newline yields one empty last element; drop it so the
	// output ends with exactly one newline.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	for i, line := range lines {
		m := includeRe.FindStringSubmatch(line)
		if m == nil {
			if strings.Contains(line, "{{include") {
				return "", fmt.Errorf("%s:%d: an include marker must be alone on its line", SourcePath, i+1)
			}
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		indent, path := m[1], m[2]
		body, err := fs.ReadFile(fsys, path)
		if err != nil {
			return "", fmt.Errorf("%s:%d: include %s: %w", SourcePath, i+1, path, err)
		}
		text := strings.TrimRight(normalize(string(body)), "\n")
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("%s:%d: include %s: file is empty", SourcePath, i+1, path)
		}
		if strings.Contains(text, "{{include") {
			return "", fmt.Errorf("%s: nested includes are not supported", path)
		}
		for _, l := range strings.Split(text, "\n") {
			if strings.TrimSpace(l) == "" {
				out.WriteByte('\n') // no trailing whitespace on blank lines
				continue
			}
			if strings.Contains(l, "\t") {
				return "", fmt.Errorf("%s: tabs are not allowed (they break YAML indentation)", path)
			}
			out.WriteString(indent)
			out.WriteString(l)
			out.WriteByte('\n')
		}
	}
	return out.String(), nil
}

// Check returns an error if the file at path differs from what Build produces.
// Line endings are ignored, so a CRLF checkout on Windows still passes.
func Check(path string) error {
	want, err := Build()
	if err != nil {
		return err
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if normalize(string(got)) == want {
		return nil
	}
	gl := strings.Split(normalize(string(got)), "\n")
	wl := strings.Split(want, "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			return fmt.Errorf("%s is stale (first difference at line %d: have %q, want %q); run: go generate ./internal/tmpl", path, i+1, g, w)
		}
	}
	return fmt.Errorf("%s is stale; run: go generate ./internal/tmpl", path)
}

// Write builds the template and writes it to path.
func Write(path string) error {
	s, err := Build()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

func normalize(s string) string {
	s = strings.TrimPrefix(s, string(rune(0xFEFF))) // a BOM some Windows editors add
	return strings.ReplaceAll(s, "\r\n", "\n")
}
