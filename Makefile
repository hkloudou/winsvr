# winsvr build helpers.
#
# Two binaries are produced:
#   - winsvr-agent  : the service (console subsystem, requireAdministrator)
#   - helper.bin    : the payload (GUI subsystem so no console flashes; asInvoker)
#
# Cross-compiling from macOS/Linux needs no C toolchain by default
# (CGO_ENABLED=0, internal linker). Set CGO=1 to use mingw for cgo builds.

GOWINRES ?= go-winres
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ARCHES   ?= amd64,386

CGO      ?= 0
GOOS      = windows
LDCOMMON  = -s -w -X main.version=$(VERSION)

# mingw compilers used only when CGO=1.
CC_amd64 ?= x86_64-w64-mingw32-gcc
CC_386   ?= i686-w64-mingw32-gcc

.PHONY: all res agent helper clean tools test vet

all: res agent helper

## res: generate .syso resources (icon + manifest + version) for both binaries
res:
	cd cmd/winsvr-agent/winres && $(GOWINRES) make --in winres.json --arch $(ARCHES) \
	  --out ../rsrc --product-version $(VERSION) --file-version $(VERSION)
	cd cmd/example-helper/winres && $(GOWINRES) make --in winres.json --arch $(ARCHES) \
	  --out ../rsrc --product-version $(VERSION) --file-version $(VERSION)

## agent: build the service binary (amd64), console subsystem
agent:
	CGO_ENABLED=$(CGO) GOOS=$(GOOS) GOARCH=amd64 $(if $(filter 1,$(CGO)),CC=$(CC_amd64),) \
	  go build -trimpath -ldflags "$(LDCOMMON)" -o dist/winsvr-agent.exe ./cmd/winsvr-agent

## helper: build the payload as helper.bin (amd64), windowless subsystem
helper:
	CGO_ENABLED=$(CGO) GOOS=$(GOOS) GOARCH=amd64 $(if $(filter 1,$(CGO)),CC=$(CC_amd64),) \
	  go build -trimpath -ldflags "$(LDCOMMON) -H windowsgui" -o dist/helper.bin ./cmd/example-helper

## helper32: same payload for 32-bit Windows
helper32:
	CGO_ENABLED=$(CGO) GOOS=$(GOOS) GOARCH=386 $(if $(filter 1,$(CGO)),CC=$(CC_386),) \
	  go build -trimpath -ldflags "$(LDCOMMON) -H windowsgui" -o dist/helper-386.bin ./cmd/example-helper

## tools: install the resource compiler
tools:
	go install github.com/tc-hib/go-winres@latest

## test: run the platform-independent unit tests
test:
	go test ./...

## vet: cross-vet for windows
vet:
	GOOS=windows GOARCH=amd64 go vet ./...
	GOOS=windows GOARCH=386   go vet ./...

clean:
	rm -rf dist
	rm -f cmd/*/rsrc_windows_*.syso
