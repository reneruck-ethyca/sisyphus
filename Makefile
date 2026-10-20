.PHONY: build test

build:
	go build -o bin/waterboy .

test:
	go test ./...
