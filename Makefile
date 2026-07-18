.PHONY: build test vet lint run docker docker-up docker-down clean

# Go binary
GO ?= go
BINARY = foxrouters

build:
	$(GO) build -ldflags="-s -w" -o $(BINARY) .

test:
	$(GO) test -count=1 -race -timeout 120s ./...

vet:
	$(GO) vet ./...

lint: vet test

run: build
	./$(BINARY)

docker:
	docker build -t foxrouters .

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

clean:
	rm -f $(BINARY)
