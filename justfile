default: test

# Every Go build needs FTS5 compiled into sqlite (for message search).
export GOFLAGS := "-tags=sqlite_fts5"

# Install dependencies, pre-commit hooks, and the playwright chromium browser.
setup:
    go mod download
    uv sync
    uv run pre-commit install
    uv run playwright install chromium

# Run the app locally on http://localhost:8080, with its data in ./dev-data. Needs SIGNAL_CLI_BIN: a signal-cli native
# binary (https://github.com/AsamK/signal-cli/releases).
run:
    mkdir -p dev-data/data dev-data/temp dev-data/sqlite
    OPENHOST_APP_DATA_DIR=dev-data/data OPENHOST_APP_TEMP_DIR=dev-data/temp OPENHOST_SQLITE_MAIN=dev-data/sqlite/main.db \
        OPENHOST_SQLITE_BRIDGE=dev-data/sqlite/bridge.db go run ./cmd/bottled-signal-client

# Fill ./dev-data with sample chats, to look at the UI without a Signal account. (`just run` then shows them, with a
# warning that signal-cli doesn't have the sample account.)
seed:
    rm -rf dev-data
    DEV_SEED_DIR="$PWD/dev-data" go test -count=1 -run TestSeedDevData ./internal/app/

# Run the Go tests, then the container tests (which need podman).
test: test-go
    uv run pytest -x

test-go:
    go test ./...

# Format, lint, and vet (same checks as the pre-commit hooks).
check:
    gofmt -w cmd internal
    go vet ./...
    uv run ruff check --fix .
    uv run ruff format .

# Download Signal's backup protos (at the libsignal commit pinned in the script) and regenerate their Go bindings.
protos:
    ./scripts/gen_protos.sh

# Move to bottled-bridge-core at ref (a commit, branch or tag; it must be pushed).
update-core ref="main":
    go get github.com/cloud-in-a-bottle/bottled-bridge-core@{{ref}}
    go mod tidy

# Build the container image.
build:
    podman build -t bottled-signal-client .
