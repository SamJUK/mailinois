VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TEST_DATABASE_URL ?= postgres://mailinois:mailinois@localhost:55432/mailinois_test

.PHONY: build test test-postgres vuln hooks up down mail site

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/mailinois ./cmd/mailinois

# SQLite always; PostgreSQL too with `make test-postgres`.
test:
	go vet ./...
	go test ./...

test-postgres:
	docker compose --profile test up -d --wait postgres
	MAILINOIS_TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# gofmt and vet before each commit, Conventional Commits on the message
hooks:
	git config core.hooksPath .githooks

up:
	docker compose up -d --build
	@echo "Dashboard: http://localhost:7480   Mailpit: http://localhost:8325   SMTP: localhost:2525"

down:
	docker compose down

# A real message through the local Postfix: make mail TO=someone@example.com
TO ?= someone@example.com
mail:
	printf 'From: Mailinois test <test@example.com>\r\nTo: <$(TO)>\r\nSubject: Mailinois test\r\nContent-Type: text/plain\r\n\r\nHello from the local relay.\r\n' \
		| curl -sS smtp://127.0.0.1:2525 --mail-from test@example.com --mail-rcpt '$(TO)' --upload-file -
	@echo "Sent to $(TO) through the local Postfix: see http://localhost:7480/messages"

# The docs and marketing site, on http://localhost:4321/mailinois/
site:
	cd site && npm install && npm run dev
