.PHONY: build test vet fmt check clean

build:
	go build -o bin/bort ./cmd/bort

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: fmt test vet build

clean:
	rm -rf bin tmp
