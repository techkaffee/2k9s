BINARY := 2k9s
PREFIX ?= $(HOME)/.local/bin

# VERSION comes from the closest git tag (e.g. v1.2.3); "dev" if there's no git/tag.
# The GitHub Actions release build (.github/workflows/release.yml) sets these
# 3 variables itself via -ldflags based on the pushed tag, no need to read them from here.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build install uninstall check clean fmt vet

## build: compile to ./2k9s
# -o is required because the module path is `twok9s`; without -o, go build
# would name the binary `twok9s`.
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

## install: build then copy into $(PREFIX) (default ~/.local/bin)
install: build
	mkdir -p $(PREFIX)
	install -m 0755 $(BINARY) $(PREFIX)/$(BINARY)
	@echo "installed -> $(PREFIX)/$(BINARY)"
	@command -v $(BINARY) >/dev/null 2>&1 \
		&& echo "OK: '$(BINARY)' is callable from PATH" \
		|| echo "NOTE: $(PREFIX) isn't in PATH yet — see the PATH section in README.md"

uninstall:
	rm -f $(PREFIX)/$(BINARY)

## check: check for aws/kubectl/k9s/fzf
check: build
	./$(BINARY) --check

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -f $(BINARY) twok9s
