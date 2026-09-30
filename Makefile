.PHONY: build test test-unit sim sim-stop evidence clean

build: ## build bin/cua and bin/legacybank
	go build -o bin/ ./cmd/cua ./cmd/legacybank

test: ## all tests; the end-to-end ones drive headless Chrome and skip without it
	go test ./...

test-unit: ## unit tests only (no browser)
	go test -short ./...

sim: build ## start both tenant simulators in the background
	./scripts/sim.sh start

sim-stop:
	./scripts/sim.sh stop

evidence: build ## regenerate everything under evidence/ (needs `make sim` and model access)
	./scripts/evidence.sh

clean:
	rm -rf bin runs
