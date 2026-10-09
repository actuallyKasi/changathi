.PHONY: build test vet
build:
	go build -o bin/changathi ./cmd/changathi
test:
	go test -race ./...
vet:
	go vet ./...
