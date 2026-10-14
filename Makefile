GO      ?= go
PKG     := ./cmd/shortener
BIN     := bin/shortener

.PHONY: run build test test-integration cover lint sec docker-up docker-down fmt

# Запуск приложения; переменные берутся из .env, если он есть.
run:
	set -a; [ -f .env ] && . ./.env; set +a; $(GO) run $(PKG)

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) $(PKG)

test:
	$(GO) test ./...

test-integration:
	$(GO) test -tags integration ./...

# Отчёт о покрытии: coverage.out и coverage.html.
cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

# Требует golangci-lint v2 (https://golangci-lint.run/welcome/install/).
lint:
	golangci-lint run

# Требует установленных gosec и govulncheck:
#   go install github.com/securego/gosec/v2/cmd/gosec@latest
#   go install golang.org/x/vuln/cmd/govulncheck@latest
sec:
	gosec ./...
	govulncheck ./...

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

fmt:
	$(GO) fmt ./...
