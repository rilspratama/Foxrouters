.PHONY: build test vet lint run docker docker-up docker-down clean release

# Go binary
GO ?= go
BINARY = foxrouters
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.Version=$(VERSION)

build:
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	$(GO) test -count=1 -race -timeout 120s ./...

vet:
	$(GO) vet ./...

lint: vet test

run: build
	./$(BINARY)

release:
	goreleaser release --clean

release-snapshot:
	goreleaser release --snapshot --clean

docker:
	docker build -t foxrouters .

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

clean:
	rm -f $(BINARY)
