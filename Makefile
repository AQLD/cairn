.PHONY: build test accept bench chaos plan run lint clean

build:
	go build -o bin/ ./cmd/...

# Unit tests and the acceptance suite (host runtime, quick loop counts).
test: lint
	go test -short -count=1 ./...

# Full acceptance run with a pass/skip summary per scenario.
accept:
	go test -count=1 -v ./acceptance -run TestEndGoals 2>&1 | grep -E -- '--- (PASS|FAIL|SKIP)' | grep -v '/self' ; \
	go test -count=1 ./acceptance

bench:
	go test -run XXX -bench . -benchmem ./bench

# Real kill -9 rounds against a cairnd process (G7.6). ROUNDS=200 make chaos
chaos:
	CAIRN_KILL_ROUNDS=$${ROUNDS:-100} go test -count=1 -timeout 0 -v ./acceptance -run 'TestEndGoals/G7_.*/G7.6' | grep -E 'g7_kill.go|--- (PASS|FAIL)'

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

# Regenerate the test plan from the scenario catalog. Commit the result.
plan:
	go run ./internal/tools/testplan > docs/test-plan.md

# Development engine: host runtime (NO isolation), localhost only.
run: build
	./bin/cairnd --runtime host --data ./data

clean:
	rm -rf bin data
