.PHONY: build run test test-one clean tidy deps docker-build docker-run

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
	rm -rf data/

# Tidy dependencies
tidy:
	go mod tidy

# Download dependencies
deps:
	go mod download

# Build Docker image
docker-build:
	docker build -t energy-trader:latest .

# Run Docker container (mounts ./data for state, uses host network for UDP)
docker-run:
	@mkdir -p data
	docker run --rm --network=host --env-file .env -v $(PWD)/data:/app/data energy-trader:latest
