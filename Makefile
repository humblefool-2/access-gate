.PHONY: bench-all run test vet fmt build agentload bench-compare bench-memory playground

# local web UI: try roles, with/without the lib, and run benchmarks (http://localhost:9090, PLAYGROUND_PORT to change)
playground:
	go run ./examples/wallet-service/playground

run:
	go run ./examples/wallet-service

test:
	go test -race ./lib ./examples/wallet-service/...

vet:
	go vet ./lib ./examples/wallet-service/... ./benchmarks/...

fmt:
	gofmt -w lib examples benchmarks

build:
	go build -o bin/wallet-service ./examples/wallet-service

# agent-shaped load against a running service (`make run` first), e.g. make agentload ARGS="-agents 20 -duration 15s"
agentload:
	go run ./benchmarks/agentload $(ARGS)

# guarded vs unguarded baseline, same agent traffic, REPS paired runs (default 5); e.g. REPS=7 make bench-compare ARGS="-agents 1 -think 0 -duration 10s"
bench-compare:
	benchmarks/compare.sh $(ARGS)

# service memory growth under sustained load (unbounded history); e.g. DURATION=10m SAMPLE=30 make bench-memory
bench-memory:
	benchmarks/memrun.sh $(ARGS)

# playground presets, store-latency samples and the memory run (about 35 min); output in benchmarks/results/<date>/
bench-all:
	benchmarks/run-all.sh $(ARGS)
