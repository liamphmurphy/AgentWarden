# GOCACHE is pinned inside the project so builds work under a restricted
# sandbox that cannot write to the default cache location.
export GOCACHE := $(CURDIR)/.gocache

BINARY := agentwarden
CMD    := ./cmd/agentwarden

# Where `make install` puts the binary. `go env GOPATH`/bin is the idiomatic
# Go location and is usually already on PATH; override with
# `make install PREFIX=/usr/local` to install to $(PREFIX)/bin instead.
GOBIN_DIR := $(shell go env GOBIN)
ifeq ($(GOBIN_DIR),)
GOBIN_DIR := $(shell go env GOPATH)/bin
endif
PREFIX  ?=
ifeq ($(PREFIX),)
INSTALL_DIR := $(GOBIN_DIR)
else
INSTALL_DIR := $(PREFIX)/bin
endif

.PHONY: help check build install uninstall where test vet fmt clean

help:
	@echo "make build      build ./$(BINARY) in this directory"
	@echo "make install    install $(BINARY) to $(INSTALL_DIR)"
	@echo "make uninstall  remove $(BINARY) from $(INSTALL_DIR)"
	@echo "make where      show the install directory and whether it is on PATH"
	@echo "make check      fmt, vet and test"
	@echo
	@echo "Override the destination:  make install PREFIX=/usr/local"

check: fmt vet test

build:
	go build -o $(BINARY) $(CMD)

# install builds and places the binary on PATH, then says so. It deliberately
# reports whether the directory is actually on PATH: installing somewhere the
# shell cannot find is the usual reason "it worked but the command is missing".
install:
	@mkdir -p "$(INSTALL_DIR)"
	go build -o "$(INSTALL_DIR)/$(BINARY)" $(CMD)
	@echo "installed $(INSTALL_DIR)/$(BINARY)"
	@case ":$$PATH:" in \
		*":$(INSTALL_DIR):"*) echo "$(INSTALL_DIR) is on PATH; run: $(BINARY)" ;; \
		*) echo; \
		   echo "WARNING: $(INSTALL_DIR) is not on your PATH."; \
		   echo "Add it, for example:"; \
		   echo "  echo 'export PATH=\"$(INSTALL_DIR):\$$PATH\"' >> ~/.zshrc && exec zsh" ;; \
	esac

uninstall:
	@rm -f "$(INSTALL_DIR)/$(BINARY)" && echo "removed $(INSTALL_DIR)/$(BINARY)"

# where answers "would an install be visible, and what am I running now?".
# Both are reported because they can differ: PATH may resolve an older copy
# from a different directory than the one make would install to.
where:
	@echo "install dir:  $(INSTALL_DIR)"
	@case ":$$PATH:" in \
		*":$(INSTALL_DIR):"*) echo "on PATH:      yes" ;; \
		*) echo "on PATH:      no" ;; \
	esac
	@printf "binary there: "; \
		if [ -x "$(INSTALL_DIR)/$(BINARY)" ]; then echo "$(INSTALL_DIR)/$(BINARY)"; else echo "(not installed)"; fi
	@printf "PATH resolves: "; command -v $(BINARY) || echo "(not found)"

test:
	go test ./... -race -cover

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BINARY)
	rm -rf .gocache
