package engine

import (
	"reflect"
	"strings"
	"testing"
)

// FuzzCheckHint: CheckHint must never panic, must be deterministic, and may
// only report the rules that belong to the requested level.
// Run: go test ./internal/engine -run '^$' -fuzz FuzzCheckHint -fuzztime 20s
func FuzzCheckHint(f *testing.F) {
	initialises := []struct {
		level int
		text  string
		ids   string
	}{
		{1, "What happens if the same request arrives twice?", ""},
		{1, "Look at app/orders/service.py", ""},
		{1, "```go\nx := 1\n```", ""},
		{2, "See the idempotency docs: https://example.com", ""},
		{2, "def handler(req):\n    return 1", ""},
		{3, "1. read the key\n2. check the store\n3. return early", "OrderService,idempotencyKey,storeLookup,saveOrder"},
		{3, "Use OrderService with idempotencyKey then storeLookup and saveOrder", "OrderService,idempotencyKey,storeLookup,saveOrder"},
		{4, "```\n" + strings.Repeat("a = 1\n", 25) + "```", ""},
		{4, "```python\n```", ""},
		{0, "", ""},
		{-1, "```", ""},
		{99, "```\n\n\n```", "x"},
		{3, "```unterminated\nfoo()\n", "self,None"},
	}
	for _, s := range initialises {
		f.Add(s.level, s.text, s.ids)
	}
	allowed := map[int]map[string]bool{
		HintNudge:   {"no_code": true, "no_files": true, "ask_question": true},
		HintPointer: {"no_code": true},
		HintOutline: {"max_lines": true, "repo_identifiers": true},
		HintAnalogy: {"max_lines": true, "repo_identifiers": true},
	}
	f.Fuzz(func(t *testing.T, level int, text, ids string) {
		idents := map[string]bool{}
		for _, id := range strings.Split(ids, ",") {
			if id != "" {
				idents[id] = true
			}
		}
		v1 := CheckHint(level, text, idents)
		v2 := CheckHint(level, text, idents)
		if !reflect.DeepEqual(v1, v2) {
			t.Fatalf("not deterministic: %v vs %v", v1, v2)
		}
		ok := allowed[level]
		if level > HintAnalogy {
			ok = map[string]bool{"repo_identifiers": true}
		}
		for _, v := range v1 {
			if !ok[v.Rule] {
				t.Fatalf("level %d reported foreign rule %q (%s)", level, v.Rule, v.Detail)
			}
			if v.Detail == "" {
				t.Fatalf("violation without detail: %+v", v)
			}
		}
		// Nothing quoted from the repo → never a repo_identifiers violation.
		for _, v := range CheckHint(level, text, nil) {
			if v.Rule == "repo_identifiers" {
				t.Fatal("repo_identifiers with no repo identifiers")
			}
		}
	})
}
