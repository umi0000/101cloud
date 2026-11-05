#!/bin/sh
set -eu

backend_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(dirname -- "$backend_dir")
frontend_only=false
if [ "${1-}" = "--frontend-only" ]; then
    frontend_only=true
    shift
    if [ "$#" -ne 0 ]; then
        echo '--frontend-only does not accept go build flags' >&2
        exit 2
    fi
fi

# Stage on the destination filesystem; never merge with an older bundle.
mkdir -p "$backend_dir/internal/webui"
stage=$(mktemp -d "$backend_dir/internal/webui/.dist-build.XXXXXX")
trap 'rm -rf "$stage"' 0
trap 'exit 1' HUP INT TERM
(
    cd "$repo_dir/frontend"
    pnpm --dir apps/web-antd exec vite build --mode production --outDir "$stage/dist" --emptyOutDir
)
if [ ! -s "$stage/dist/index.html" ] || [ ! -s "$stage/dist/_app.config.js" ]; then
    echo 'Frontend build must produce nonempty index.html and _app.config.js' >&2
    exit 1
fi
# Preserve the prior bundle if building or validation failed above.
if [ -e "$backend_dir/internal/webui/dist" ]; then
    mv "$backend_dir/internal/webui/dist" "$stage/previous"
fi
if ! mv "$stage/dist" "$backend_dir/internal/webui/dist"; then
    if [ -d "$stage/previous" ]; then
        mv "$stage/previous" "$backend_dir/internal/webui/dist"
    fi
    exit 1
fi

if [ "$frontend_only" = true ]; then
    exit 0
fi
cd "$backend_dir"
mkdir -p bin
go build -trimpath -o bin/101cloud-backend "$@" ./cmd/backend
