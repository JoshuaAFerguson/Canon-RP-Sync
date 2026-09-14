.PHONY: build test lint docker

build:
	go build -o bin/rpsync ./cmd/rpsync

test:
	go test ./...

lint:
	go vet ./...

docker:
	docker build -t canon-rp-sync -f deploy/Dockerfile .
