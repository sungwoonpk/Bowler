.PHONY: build test race cover lint fmt vet vuln demo docker check clean

build:
	go build -trimpath -o bin/bowler ./cmd/bowler

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

# Coverage of the libraries. The command package is exercised by tests that start it as a separate process, which
# a coverage tool cannot see, so it is left out of this number.
cover:
	go test ./internal/... ./retry/... ./signature/... -count=1 -coverprofile=cover.out -coverpkg=./internal/...,./retry,./signature
	go tool cover -func=cover.out | tail -1

fmt:
	gofmt -w .

vet:
	go vet ./...

lint:
	@test -z "$$(gofmt -l .)" || (echo "these files need gofmt:"; gofmt -l .; exit 1)
	go vet ./...
	staticcheck ./...

vuln:
	govulncheck ./...

demo: build
	./bin/bowler demo

docker:
	docker build -t bowler:dev .

# Everything CI runs, in the order it runs it.
check: lint race cover vuln

clean:
	rm -f bin/bowler cover.out
