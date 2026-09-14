VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint fmt docker run clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/rpsync ./cmd/rpsync

test:
	go test -race ./...

lint:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi

fmt:
	gofmt -w .

# Builds the image for the host architecture. Release images are multi-arch;
# see .github/workflows/release.yml.
docker:
	docker build -t canon-rp-sync:$(VERSION) --build-arg VERSION=$(VERSION) -f deploy/Dockerfile .

run: build
	./bin/rpsync daemon

clean:
	rm -rf bin dist
