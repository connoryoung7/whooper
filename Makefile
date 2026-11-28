# Simple Makefile for a Go project

# Build the application
all: build test

build:
	@echo "Building..."
	
	
	@go build -o main ./cmd/whooper

# Run the web server
run:
	@go run ./cmd/whooper serve

# Sync WHOOP data once (FULL=1 re-downloads the whole history)
sync:
	@go run ./cmd/whooper sync $(if $(FULL),--full)

# Print the data completeness report
status:
	@go run ./cmd/whooper status

# Create a new database migration: make migration name=add_something
# (uses the goose version pinned in go.mod; migrations apply automatically on startup)
GOOSE_VERSION = $(shell go list -m -f '{{.Version}}' github.com/pressly/goose/v3)
migration:
	@test -n "$(name)" || (echo "usage: make migration name=<snake_case_name>" && exit 1)
	@go run github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION) -dir internal/database/migrations -s create $(name) sql

# Build and run the full stack (Postgres, api, cron)
docker-run:
	@if docker compose up --build 2>/dev/null; then \
		: ; \
	else \
		echo "Falling back to Docker Compose V1"; \
		docker-compose up --build; \
	fi

# Shut down the stack
docker-down:
	@if docker compose down 2>/dev/null; then \
		: ; \
	else \
		echo "Falling back to Docker Compose V1"; \
		docker-compose down; \
	fi

# Test the application
test:
	@echo "Testing..."
	@go test ./... -v
# Integrations Tests for the application
itest:
	@echo "Running integration tests..."
	@go test ./internal/database ./internal/syncer ./internal/server -v

# Clean the binary
clean:
	@echo "Cleaning..."
	@rm -f main

# Live Reload
watch:
	@if command -v air > /dev/null; then \
            air; \
            echo "Watching...";\
        else \
            read -p "Go's 'air' is not installed on your machine. Do you want to install it? [Y/n] " choice; \
            if [ "$$choice" != "n" ] && [ "$$choice" != "N" ]; then \
                go install github.com/air-verse/air@latest; \
                air; \
                echo "Watching...";\
            else \
                echo "You chose not to install air. Exiting..."; \
                exit 1; \
            fi; \
        fi

.PHONY: all build run sync status migration test clean watch docker-run docker-down itest
