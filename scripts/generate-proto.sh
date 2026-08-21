#!/usr/bin/env bash
#
# Generate the Go code for the worker protocol.
#
# buf rather than protoc. buf is one Go program that `go install` fetches, so
# this script works on any machine that can build the project. protoc is a C++
# binary that arrives from a package manager, and the version it gives differs
# between machines, which is how a repository ends up with generated code that
# nobody can reproduce.
#
# The output is committed. CI runs this script and fails if the result differs
# from what is in the tree.

set -euo pipefail

cd "$(dirname "$0")/.."

# Pinned. An unpinned plugin regenerates a different file the day it is
# released, and the difference arrives in an unrelated pull request.
BUF_VERSION="v1.47.2"
PROTOC_GEN_GO_VERSION="v1.36.6"
PROTOC_GEN_GO_GRPC_VERSION="v1.5.1"

TOOLS="$(go env GOPATH)/bin"
export PATH="$TOOLS:$PATH"

need() {
	local name="$1" module="$2"
	if ! command -v "$name" > /dev/null; then
		echo "Installing $name"
		go install "$module"
	fi
}

need buf "github.com/bufbuild/buf/cmd/buf@${BUF_VERSION}"
need protoc-gen-go "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
need protoc-gen-go-grpc "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"

echo "Checking the protocol"
buf lint

echo "Generating"
buf generate

echo "Done. The generated files are in internal/quorrapb."
