BINARY  := nocturne
VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/lightight/nocturnecli/internal/app.Version=$(VERSION)
PREFIX  ?= $(HOME)/.local

.PHONY: build install run dist clean tidy

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: build
	mkdir -p $(PREFIX)/bin
	cp $(BINARY) $(PREFIX)/bin/$(BINARY)
	@echo "installed to $(PREFIX)/bin/$(BINARY)"

run:
	go run .

tidy:
	go mod tidy

# Cross-compile static binaries for every supported platform into ./dist
dist:
	@mkdir -p dist
	@for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; out=dist/$(BINARY)_$${os}_$${arch}; \
		[ "$$os" = "windows" ] && out=$$out.exe; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
	done

clean:
	rm -rf $(BINARY) dist
