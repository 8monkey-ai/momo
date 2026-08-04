.PHONY: build test lint check

build:
	go build ./...

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt -w ."; exit 1; }
	go vet ./...
	golangci-lint run

check: lint test build
