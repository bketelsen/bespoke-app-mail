_default:
    @just --list --unsorted

# Regenerate templ output (COMMIT it — instances build from the module cache)
ui:
    go tool templ generate

# Same recipe CI runs. Run before calling work done.
check:
    go vet ./...
    go test ./...
    golangci-lint run
    go mod tidy -diff
    CGO_ENABLED=0 GOOS=linux go build ./...
