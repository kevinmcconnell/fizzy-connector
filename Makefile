.PHONY: build test lint

build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...

test:
	go test ./...

lint:
	golangci-lint run
