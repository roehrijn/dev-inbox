set positional-arguments

build:
    go build -o bin/dev-inbox ./cmd/dev-inbox

test *args='./...':
    go test "$@"

lint *args='./...':
    go vet "$@"
    golangci-lint run "$@"

fmt:
    gofmt -w .
