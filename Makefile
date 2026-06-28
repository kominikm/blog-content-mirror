COVERPROFILE ?= coverage.out
COVERHTML ?= coverage.html

.PHONY: fmt modernise test race vet staticcheck coverage coverage-html check

fmt:
	test -z "$$(gofmt -l .)"

modernise:
	go fix -diff ./...

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

staticcheck:
	staticcheck ./...

coverage:
	go test -coverprofile=$(COVERPROFILE) ./...
	go tool cover -func=$(COVERPROFILE)

coverage-html: coverage
	go tool cover -html=$(COVERPROFILE) -o $(COVERHTML)

check: fmt modernise vet staticcheck test race
