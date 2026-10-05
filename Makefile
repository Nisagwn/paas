-include .env
export

.PHONY: db registry buildkitd run test test-build test-unit build cli vet clean

db:            ## start PostgreSQL in Docker
	docker compose up -d --wait postgres

registry:      ## start the local image registry (localhost:5000)
	docker compose up -d --wait registry

buildkitd: registry  ## start a standalone buildkitd (for PAAS_BUILDER=buildkit)
	docker compose --profile buildkit up -d buildkitd

run: db registry  ## run the control plane locally
	go run ./cmd/paas

build:
	CGO_ENABLED=0 go build -o bin/paas ./cmd/paas

cli:           ## build the paas CLI (Faz 18) into bin/paas-cli
	CGO_ENABLED=0 go build -o bin/paas-cli ./cmd/paas-cli

vet:
	go vet ./...

test-unit:     ## tests that need no database
	go test ./...

# -p 1: packages share one test database, so they must not run in parallel.
test: db vet   ## all tests, including database integration tests
	go test -p 1 -race -count=1 ./...

# Real image builds of examples/ with Docker, pushed to the local registry.
test-build: registry
	PAAS_TEST_DOCKER_BUILD=1 go test -count=1 -run Docker -v ./internal/build/

clean:
	rm -rf bin
	docker compose down -v
