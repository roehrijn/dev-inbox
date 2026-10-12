set positional-arguments

build:
    go build -o bin/dev-inbox ./cmd/dev-inbox
    go build -o bin/dev-inbox-jev ./cmd/dev-inbox-jev
    go build -o bin/dev-inbox-linear ./cmd/dev-inbox-linear

test *args='./...':
    go test "$@"

lint *args='./...':
    go vet "$@"
    golangci-lint run "$@"

fmt:
    gofmt -w .
