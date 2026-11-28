.PHONY: all build web-build go-build release test vet check clean

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

all: build

web-build:
	cd web && npm run typecheck && npm test && npm run build

go-build:
	go build -ldflags "$(LDFLAGS)" -o bin/corgi ./cmd/corgi

build: web-build go-build

release:
	VERSION=$(VERSION) sh scripts/build-release.sh

test:
	go test -race ./...

vet:
	go vet ./...

check: vet test
	cd web && npm run typecheck && npm test

clean:
	rm -rf bin/ web/dist/*
	touch web/dist/.gitkeep
