.PHONY: build run test test-one clean tidy deps docker-build docker-run docker-migrate-data

# Build the binary
build:
	go build -o energy-trader ./cmd/trader

# Run locally
run:
	go run ./cmd/trader

# Run tests
test:
	go test -race -v ./...

# Run a single test
test-one:
	go test -race -v ./... -run $(TEST)

# Clean build artifacts
clean:
	rm -f energy-trader

# Tidy dependencies
tidy:
	go mod tidy

# Download dependencies
deps:
	go mod download

# Build Docker image
docker-build:
	docker build -t energy-trader:latest .

# Run Docker container (persists state and permits access to local devices/mDNS)
docker-run:
	docker run -d --name energy-trader --restart unless-stopped --stop-timeout 95 --network=host --env-file .env --mount type=volume,src=energy-trader-data,dst=/app/data energy-trader:latest

# One-time migration from the pre-volume ./data bind mount. Refuses overwrite.
docker-migrate-data:
	@test -d data || (printf '%s\n' 'data/ does not exist' >&2; exit 1)
	@if [ "$$(docker inspect -f '{{.State.Running}}' energy-trader 2>/dev/null)" = true ]; then printf '%s\n' 'energy-trader is running; stop it before migration' >&2; exit 1; fi
	@docker volume create energy-trader-data >/dev/null
	docker run --rm --user 0:0 --entrypoint /bin/sh -v "$(PWD)/data:/source:ro,z" --mount type=volume,src=energy-trader-data,dst=/dest energy-trader:latest -c 'set -eu; test -z "$$(ls -A /dest)" || { printf "%s\n" "energy-trader-data is not empty; refusing overwrite" >&2; exit 1; }; trap '\''status=$$?; trap - EXIT; if [ "$$status" -ne 0 ]; then rm -rf /dest/?* /dest/.[!.]* /dest/..?*; fi; exit "$$status"'\'' EXIT; cp -a /source/. /dest/; chown -R 1000:1000 /dest'
