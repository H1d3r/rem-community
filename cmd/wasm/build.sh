#!/bin/bash

# Build script for WASM rem client

set -e

echo "Building WASM rem client..."

# Set environment variables for WASM build
export GOOS=js
export GOARCH=wasm

# Build the WASM binary
echo "Compiling Go to WASM..."
go build -o static/main.wasm main.go

# Copy wasm_exec.js from Go installation
echo "Copying wasm_exec.js..."
cp "$(go env GOROOT)/misc/wasm/wasm_exec.js" static/

echo "Build completed successfully!"
echo "Files generated:"
echo "  - static/main.wasm"
echo "  - static/wasm_exec.js"
echo "  - static/index.html"
echo ""
echo "To test, run the server:"
echo "  cd server && go run server.go"
echo "Then open: https://localhost:8080/static/index.html"
