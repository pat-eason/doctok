VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test vet fmt clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/doctok ./cmd/doctok

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/doctok

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin
