.PHONY: build run test cover vet fmt

build:
	go build -o bin/checkin-board ./cmd/checkin-board

run:
	go run ./cmd/checkin-board

test:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

vet:
	go vet ./...

fmt:
	gofmt -w .
