.PHONY: build test accept bench plan run lint clean

build:
	go build -o bin/ ./cmd/...

# Unit tests and the acceptance suite (host runtime, quick loop counts).
test:
	go vet ./...
	go test -short -count=1 ./...

# Full acceptance run with a pass/skip summary per scenario.
accept:
	go test -count=1 -v ./acceptance -run TestEndGoals 2>&1 | grep -E -- '--- (PASS|FAIL|SKIP)' | grep -v '/self' ; \
	go test -count=1 ./acceptance

bench:
	go test -run XXX -bench . -benchmem ./bench

# Regenerate the test plan from the scenario catalog. Commit the result.
plan:
	go run ./internal/tools/testplan > docs/test-plan.md

# Development engine: host runtime (NO isolation), localhost only.
run: build
	./bin/cairnd --runtime host --data ./data

clean:
	rm -rf bin data
