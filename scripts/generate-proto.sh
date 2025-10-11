#!/bin/bash

# Generate Go code from protobuf definitions
# Requires protoc and protoc-gen-go-grpc to be installed

set -e

echo "Generating protobuf code..."

protoc --go_out=. --go_opt=paths=source_relative \
    --go-grpc_out=. --go-grpc_opt=paths=source_relative \
    proto/quorra.proto

echo "Protobuf code generated successfully!"
