.PHONY: build pi dist run test jstest e2e soak fuzz cover contract vet fmt

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/checkin-board ./cmd/checkin-board

# Pi Zero W (ARMv6). Pure Go, so no cross toolchain is needed.
pi:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/checkin-board-armv6 ./cmd/checkin-board

# Install bundle: Linux binaries for armv6 (any Raspberry Pi), arm64 and
# amd64, the unit, env template, install script and docs. install.sh
# picks the binary for the machine it runs on.
DIST := dist/checkin-board-$(VERSION)
dist:
	rm -rf $(DIST) && mkdir -p $(DIST)/bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/bin/checkin-board-linux-armv6 ./cmd/checkin-board
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/bin/checkin-board-linux-arm64 ./cmd/checkin-board
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/bin/checkin-board-linux-amd64 ./cmd/checkin-board
	cp deploy/checkin-board.service deploy/checkin-board-panel.service deploy/checkin-board.env deploy/install.sh docs/operator-guide.md docs/linkcheck-action.md README.md $(DIST)/
	cd $(DIST) && shasum -a 256 bin/* > SHA256SUMS
	COPYFILE_DISABLE=1 tar -C dist -czf $(DIST)-linux.tar.gz checkin-board-$(VERSION)

run:
	go run ./cmd/checkin-board

test:
	go test -race ./...

jstest:
	node --test internal/web/jstest/

e2e:
	CB_E2E=1 go test -count=1 -run '^TestE2E' ./internal/web/

soak:
	CB_SOAK=1 go test -count=1 -run '^TestSoak$$' -v -timeout 60m ./internal/sim/

# Long fuzz runs (spec 12b): FUZZTIME per target, default 30m.
FUZZTIME ?= 30m
fuzz:
	go test -run XXX -fuzz FuzzDecode -fuzztime $(FUZZTIME) ./internal/wire/
	go test -run XXX -fuzz FuzzParseRosterCSV -fuzztime $(FUZZTIME) ./internal/store/
	go test -run XXX -fuzz FuzzProcessLogo -fuzztime $(FUZZTIME) ./internal/branding/
	go test -run XXX -fuzz 'FuzzParse$$' -fuzztime $(FUZZTIME) ./internal/journal/
	go test -run XXX -fuzz FuzzParseExport -fuzztime $(FUZZTIME) ./internal/ops/

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

contract:
	go test -v -count=1 -timeout 15m -run '^TestContract' ./internal/graywolf/

vet:
	go vet ./...

fmt:
	gofmt -w .
