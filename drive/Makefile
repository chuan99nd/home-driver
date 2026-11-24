# Drive: single static binary, no cgo.
GO ?= go
export GOTOOLCHAIN ?= local

.PHONY: build test race lint fmt clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o bin/drive ./cmd/drive

test:
	$(GO) test ./...

race:
	CGO_ENABLED=1 $(GO) test -race ./...

lint:
	$(GO) vet ./...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

fmt:
	gofmt -w .

clean:
	$(GO) clean
	rm -f bin/drive
