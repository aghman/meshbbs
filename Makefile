MODULE  := github.com/aghman/meshbbs
BINARY  := meshbbs
VERSION := $(shell git rev-parse --short=8 HEAD 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/cli.Version=$(VERSION)

.PHONY: build test test-race vet fmt fmt-check check conformance vectors dict cross clean docs

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/meshbbs
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY)-key ./cmd/meshbbs-key

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l . | grep -v '^internal/identity/wordlist.go$$' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

# The wire-format gate (§12.6). Separated from `test` so that "the bytes on the
# mesh changed" can be asked as its own question.
conformance:
	go test ./internal/conformance/...

# Append new vectors to the frozen corpus. Refuses to alter an existing one.
vectors:
	go run ./tools/conformance

# Retrain the compression dictionary (§7.4). Refuses to overwrite one that
# exists: a dictionary is superseded under a new ID, never edited in place.
dict:
	go run ./tools/traindict

check: fmt-check vet conformance test-race

# Regenerate the two documentation artifacts built from the config struct tags,
# so neither can drift from the binary. Run after changing a config key.
docs:
	go run ./cmd/meshbbs config reference --markdown > docs/config.md
	go run ./tools/genconfigsite

# Cross-compile for all platforms shipped by CI.
cross:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64     ./cmd/meshbbs
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-key-linux-amd64     ./cmd/meshbbs-key
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64     ./cmd/meshbbs
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-key-linux-arm64     ./cmd/meshbbs-key
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-amd64    ./cmd/meshbbs
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-key-darwin-amd64    ./cmd/meshbbs-key
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64    ./cmd/meshbbs
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-key-darwin-arm64    ./cmd/meshbbs-key
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe ./cmd/meshbbs
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-key-windows-amd64.exe ./cmd/meshbbs-key

clean:
	rm -rf $(BINARY) dist
