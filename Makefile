# plait — every quality gate runs through `make check`.
SHELL := /bin/bash
GO ?= go
export GOFLAGS ?= -p=3
COVER_MIN ?= 80
COVERPKG := ./internal/...

.PHONY: check fmt fmt-check vet lint test cover build demo bench fuzz loc clean

check: fmt-check vet lint cover ## format, vet (type-check), lint, race tests + coverage gate

fmt:
	gofmt -w cmd internal test

fmt-check:
	@out="$$(gofmt -l cmd internal test)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

test:
	$(GO) test -race ./...

# Race-enabled tests with cross-package coverage of the core packages.
cover:
	$(GO) test -race -coverpkg=$(COVERPKG) -coverprofile=coverage.out ./...
	@scripts/coverage.sh coverage.out $(COVER_MIN)

build:
	$(GO) build -o bin/plait ./cmd/plait

demo: build
	@scripts/demo.sh

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/engine ./internal/dag

fuzz:
	$(GO) test -run '^$$' -fuzz FuzzParseFile -fuzztime 30s ./internal/syntax
	$(GO) test -run '^$$' -fuzz FuzzJSONRoundTrip -fuzztime 30s ./internal/value

# Line counts quoted in the README (all lines, including comments and blanks).
loc:
	@printf 'non-test Go source: '; find cmd internal -name '*.go' ! -name '*_test.go' | xargs cat | wc -l
	@printf 'test Go source:     '; find cmd internal test -name '*_test.go' | xargs cat | wc -l

clean:
	rm -rf bin coverage.out coverage.html demo-work
