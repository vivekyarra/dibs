GO      ?= go
BIN     ?= dibs
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
COVER_MIN ?= 75

.PHONY: all build test race lint cover demo template fmt clean

all: fmt lint test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/dibs

test:
	$(GO) test -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

lint:
	golangci-lint run ./...

cover:
	$(GO) test -count=1 -covermode=atomic -coverprofile=coverage.out ./...
	@total=$$($(GO) tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$$3); print $$3}'); \
	echo "Total coverage: $$total%"; \
	awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { exit (t + 0 >= m) ? 0 : 1 }' || \
	{ echo "Coverage $$total% is below $(COVER_MIN)%"; exit 1; }

demo:
	$(GO) run ./cmd/dibs demo --out demo-out

template:
	$(GO) run ./cmd/dibs template --check

fmt:
	gofmt -w .

clean:
	rm -rf $(BIN) $(BIN).exe dist/ site/ demo-out/ coverage.out
