# Version of the binary. Taken from an exact Git tag on HEAD, so a checkout of
# v1.0.0 builds a binary reporting 1.0.0. Off a tag it is empty, which keeps
# the value compiled into internal/version.Version.
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null | sed "s/^v//")
# GNU make does not read ** as a recursive glob. Listing the sources with find
# keeps a rebuild triggered by a file at any depth.
SOURCES := $(shell find cmd internal -type f -name '*.go')
LDFLAGS :=
ifneq ($(VERSION),)
LDFLAGS := -X github.com/AviaOne/gnovisor/internal/version.Version=$(VERSION)
endif

all: build

# build binaries for current platform
build: build/gnovisor

build/gnovisor: $(SOURCES) go.mod
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o ./build/gnovisor ./cmd/gnovisor

# build linux binaries
build-linux: build/gnovisor.elf

build/gnovisor.elf: $(SOURCES) go.mod
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$(LDFLAGS)" -o ./build/gnovisor.elf ./cmd/gnovisor

test:
	go test -race ./...

lint:
	@echo "--> Running linter"
	@golangci-lint run ./...

clean:
	rm -rf build

.PHONY: all clean test lint build-linux build
