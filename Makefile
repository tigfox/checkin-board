.PHONY: build pi dist run test jstest e2e cover contract vet fmt

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/checkin-board ./cmd/checkin-board

# Pi Zero W (ARMv6). Pure Go, so no cross toolchain is needed.
pi:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/checkin-board-armv6 ./cmd/checkin-board

# Release bundle for the Pi: binary + unit + install script.
dist: pi
	rm -rf dist/checkin-board-$(VERSION) && mkdir -p dist/checkin-board-$(VERSION)
	cp bin/checkin-board-armv6 dist/checkin-board-$(VERSION)/checkin-board
	cp deploy/checkin-board.service deploy/checkin-board.env deploy/install.sh docs/operator-guide.md docs/linkcheck-action.md dist/checkin-board-$(VERSION)/
	COPYFILE_DISABLE=1 tar -C dist -czf dist/checkin-board-$(VERSION)-linux-armv6.tar.gz checkin-board-$(VERSION)

run:
	go run ./cmd/checkin-board

test:
	go test -race ./...

jstest:
	node --test internal/web/jstest/

e2e:
	CB_E2E=1 go test -count=1 -run '^TestE2E' ./internal/web/

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

contract:
	go test -v -count=1 -timeout 15m -run '^TestContract' ./internal/graywolf/

vet:
	go vet ./...

fmt:
	gofmt -w .
