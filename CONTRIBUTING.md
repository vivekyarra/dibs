# Contributing to dibs

## Setup

- Go (version from `go.mod`)
- [golangci-lint](https://golangci-lint.run/) v2 for `make lint`
- [uv](https://docs.astral.sh/uv/) if you touch `examples/demo-shop/`

```sh
git clone https://github.com/vivekyarra/dibs
cd dibs
make build      # produces ./dibs
make test       # go test ./...
```

Other targets: `make race`, `make lint`, `make cover` (fails under 75%), `make demo` (offline demo into `demo-out/`), `make template` (checks `switch/dibs.template.yaml` is up to date), `make fmt`, `make clean`.

For `examples/demo-shop/`: `cd examples/demo-shop && uv run pytest -q`.

## Branch and PR flow

1. Branch from `main`: `git switch -c feat/short-description`.
2. Keep changes focused; add or update tests.
3. Run `make fmt lint test` before pushing.
4. Open a PR against `main` and fill in the template. CI must be green before merge.

## Commit style

Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `ci:`, `chore:`, with an optional scope, e.g. `feat(guard): refuse cross-vendor hints`. The release notes are grouped from these prefixes.
