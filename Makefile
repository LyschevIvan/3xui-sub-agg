BINARY := bin/aggregator
PKG    := ./cmd/aggregator
CONFIG ?= config.example.yaml

.PHONY: all build run test fmt tidy lint clean docker docker-run

all: build

build:
	go build -o $(BINARY) $(PKG)

run:
	go run $(PKG) -config $(CONFIG)

test:
	go test ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

lint:
	golangci-lint run

clean:
	rm -rf bin

docker:
	docker build -t 3xui-sub-agg:dev .

docker-run:
	docker run --rm -p 8080:8080 -v $(PWD)/$(CONFIG):/app/config.yaml:ro 3xui-sub-agg:dev
