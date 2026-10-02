-include .env
export

.PHONY: db run test test-unit build vet clean

db:            ## start PostgreSQL in Docker
	docker compose up -d --wait postgres

run: db        ## run the control plane locally
	go run ./cmd/minipaas

build:
	CGO_ENABLED=0 go build -o bin/minipaas ./cmd/minipaas

vet:
	go vet ./...

test-unit:     ## tests that need no database
	go test ./...

# -p 1: packages share one test database, so they must not run in parallel.
test: db vet   ## all tests, including database integration tests
	go test -p 1 -race -count=1 ./...

clean:
	rm -rf bin
	docker compose down -v
