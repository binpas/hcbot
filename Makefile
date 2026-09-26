# HEALTH bot build. Run ./configure first to choose the platform and the
# install folder (config.mk); without it, the defaults below are used.
# Works with GNU Make 3.81 (macOS) and later.

-include config.mk

GO     ?= go
GOOS   ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
BINDIR ?= /usr/local/bin

VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOBUILD  = CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)"

INSTANCES := $(basename $(notdir $(wildcard instances/*.conf)))

.PHONY: all build linux test check install instance services install-instance clean distclean help

all: build

help:
	@echo "make                  build gohealthy and markovtrain for $(GOOS)/$(GOARCH)"
	@echo "make linux            build both for botden (linux/amd64) into dist/linux-amd64/"
	@echo "make test             go test ./..."
	@echo "make check            gofmt, go vet, tests, and golangci-lint if installed"
	@echo "make install          copy both binaries to $(BINDIR) (use sudo)"
	@echo "make instance NAME=x  ask for x's user, group and folder; make build/x.service"
	@echo "make services         make every build/<name>.service again from instances/"
	@echo "make install-instance NAME=x"
	@echo "                      install x's folder and unit on the server (use sudo)"
	@echo "make clean            remove the binaries, dist/ and build/"
	@echo "make distclean        also remove config.mk"

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GOBUILD) -o gohealthy ./cmd/gohealthy
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GOBUILD) -o markovtrain ./cmd/markovtrain

linux:
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o dist/linux-amd64/gohealthy ./cmd/gohealthy
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o dist/linux-amd64/markovtrain ./cmd/markovtrain

test:
	$(GO) test ./...

check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt: these files need formatting:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) test ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else echo "golangci-lint is not installed; skipped."; fi

# install does not build, so `sudo make install` never runs Go as root.
install:
	@for f in gohealthy markovtrain; do \
		[ -f $$f ] || { echo "$$f is not built. Run make first."; exit 1; }; done
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 gohealthy markovtrain $(DESTDIR)$(BINDIR)/

instance:
	@[ -n "$(NAME)" ] || { echo "Usage: make instance NAME=<name> [BOTUSER=u] [BOTGROUP=g] [RUNDIR=/path]"; exit 2; }
	@BINDIR='$(BINDIR)' BOTUSER='$(BOTUSER)' BOTGROUP='$(BOTGROUP)' RUNDIR='$(RUNDIR)' sh deploy/instance.sh '$(NAME)'

services:
	@[ -n "$(INSTANCES)" ] || { echo "No instances yet. Run: make instance NAME=<name>"; exit 1; }
	@for n in $(INSTANCES); do BINDIR='$(BINDIR)' sh deploy/instance.sh --render "$$n" || exit 1; done

install-instance:
	@[ -n "$(NAME)" ] || { echo "Usage: sudo make install-instance NAME=<name>"; exit 2; }
	@BINDIR='$(BINDIR)' sh deploy/instance.sh --install '$(NAME)'

clean:
	rm -f gohealthy markovtrain
	rm -rf dist build

distclean: clean
	rm -f config.mk
