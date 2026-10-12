set positional-arguments

build:
    go build -o bin/dev-inbox ./cmd/dev-inbox
    go build -o bin/dev-inbox-jev ./cmd/dev-inbox-jev

test *args='./...':
    go test "$@"

lint *args='./...':
    go vet "$@"
    golangci-lint run "$@"

fmt:
    gofmt -w .
