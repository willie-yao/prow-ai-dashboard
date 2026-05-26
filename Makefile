.PHONY: all build test test-v lint fmt tidy \
       fetch-data fetch-data-quick fetch-data-ai fetch-data-ai-quick \
       fe-install dev fe-build fe-check \
       dist dist-ai clean clean-cache clean-all deploy help

# Default target
all: build

## ─── Go Backend ───────────────────────────────────────────────

# Build the data fetcher binary
build:
	cd backend && go build -o ../bin/fetcher ./cmd/fetcher/

# Run all Go tests
test:
	cd backend && go test ./... -count=1

# Run Go tests with verbose output
test-v:
	cd backend && go test ./... -count=1 -v

# Run Go linter (requires golangci-lint)
lint:
	cd backend && golangci-lint run ./...

# Format Go code
fmt:
	cd backend && gofmt -w .

# Tidy Go modules
tidy:
	cd backend && go mod tidy

## ─── Data Fetching ────────────────────────────────────────────

# Fetch fresh test data from GCS into frontend/public/data/
fetch-data: build
	./bin/fetcher -builds=8 -workers=5 -out=frontend/public/data -timeout=5m

# Fetch minimal data (3 builds per job, faster)
fetch-data-quick: build
	./bin/fetcher -builds=3 -workers=5 -out=frontend/public/data -timeout=3m

# Fetch data with AI analysis (requires AI_TOKEN env var)
fetch-data-ai: build
	./bin/fetcher -builds=8 -workers=5 -out=frontend/public/data -timeout=30m -ai

# Fetch minimal data with AI analysis
fetch-data-ai-quick: build
	./bin/fetcher -builds=3 -workers=5 -out=frontend/public/data -timeout=5m -ai

## ─── Frontend ─────────────────────────────────────────────────

# Install frontend dependencies
fe-install:
	cd frontend && npm ci

# Start the Vite dev server
dev: fe-install
	cd frontend && npm run dev

# Build the frontend for production
fe-build: fe-install
	cd frontend && npm run build

# TypeScript type check
fe-check:
	cd frontend && npx tsc --noEmit

## ─── Full Pipeline ────────────────────────────────────────────

# Build everything: Go binary + fetch data + frontend
dist: fetch-data fe-build

# Build everything with AI analysis
dist-ai: fetch-data-ai fe-build

# Clean build artifacts and generated data
clean:
	rm -rf bin/ frontend/dist frontend/public/data/dashboard.json frontend/public/data/jobs/ frontend/public/data/flakiness.json

# Clean AI analysis cache (forces re-analysis on next fetch)
clean-cache:
	rm -f frontend/public/data/ai_cache.json

# Clean everything including cache
clean-all: clean clean-cache

# Trigger GitHub Actions deploy workflow
deploy:
	gh workflow run deploy.yml

## ─── Help ─────────────────────────────────────────────────────

help:
	@echo "CAPZ Prow Dashboard — Make Targets"
	@echo ""
	@echo "  build              Build Go data fetcher binary"
	@echo "  test               Run Go tests"
	@echo "  test-v             Run Go tests (verbose)"
	@echo "  lint               Run golangci-lint"
	@echo "  fmt                Format Go code"
	@echo "  tidy               Tidy Go modules"
	@echo ""
	@echo "  fetch-data         Fetch data from GCS (15 builds/job)"
	@echo "  fetch-data-quick   Fetch minimal data (3 builds/job)"
	@echo "  fetch-data-ai      Fetch data + AI analysis (needs AI_TOKEN)"
	@echo "  fetch-data-ai-quick  Fetch minimal data + AI analysis"
	@echo ""
	@echo "  fe-install         Install frontend npm dependencies"
	@echo "  dev                Start Vite dev server"
	@echo "  fe-build           Production build of frontend"
	@echo "  fe-check           TypeScript type check"
	@echo ""
	@echo "  dist               Full pipeline: build + fetch + frontend"
	@echo "  dist-ai            Full pipeline with AI analysis"
	@echo "  clean              Remove build artifacts and data"
	@echo "  clean-cache        Clear AI analysis cache"
	@echo "  clean-all          Clean everything including cache"
	@echo "  deploy             Trigger GitHub Actions deploy"
