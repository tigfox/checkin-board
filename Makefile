.PHONY: build pi run test cover contract vet fmt

build:
	go build -o bin/checkin-board ./cmd/checkin-board

pi:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -o bin/checkin-board-armv6 ./cmd/checkin-board

run:
	go run ./cmd/checkin-board

test:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

contract:
	go test -v -count=1 -timeout 15m -run '^TestContract' ./internal/graywolf/

vet:
	go vet ./...

fmt:
	gofmt -w .
