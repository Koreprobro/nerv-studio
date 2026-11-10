# nerv-core — build the MCP daemon.
#
# Layout: source lives in src/, the daemon is emitted to bin/. The daemon
# resolves every relative domain path (nerv.json "domains") against the
# project root, which it derives from its own location (parent of bin/);
# see config.ProjectRoot. Keeping source and binary
# in separate trees cleanly separates the dev tree from the running service.
#
# Targets: make build | make test | make vet | make clean

ROOT   := $(CURDIR)
SRC    := $(ROOT)/src/nerv-core
BINDIR := $(ROOT)/bin
BINARY := nerv-core

.PHONY: all build test vet clean

all: build

build:
	@mkdir -p $(BINDIR)
	cd $(SRC) && go build -o $(BINDIR)/$(BINARY) ./cmd/nerv-core

test:
	cd $(SRC) && go test ./...

vet:
	cd $(SRC) && go vet ./...

clean:
	rm -f $(BINDIR)/$(BINARY)
