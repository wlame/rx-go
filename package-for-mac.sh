#!/usr/bin/env bash
#
# Package RX benchmark for ARM MacBook
#
# Creates a portable package with:
# - ARM64 macOS binary
# - Benchmark script
# - Documentation
#

set -euo pipefail

PACKAGE_NAME="rx-benchmark-mac-$(date +%Y%m%d-%H%M%S).tar.gz"

echo "Creating benchmark package for ARM MacBook..."
echo ""

# Check if binary exists
if [[ ! -f "dist/rx-darwin-arm64" ]]; then
    echo "Error: ARM64 macOS binary not found!"
    echo "Building it now..."
    make build-darwin-arm64
fi

# Create package
tar -czf "$PACKAGE_NAME" \
    dist/rx-darwin-arm64 \
    test/comparison/wiki-benchmark.sh \
    test/comparison/BENCHMARK_GUIDE.md \
    --transform 's|^|rx-benchmark/|'

echo "✓ Package created: $PACKAGE_NAME"
echo ""
echo "Package contents:"
tar -tzf "$PACKAGE_NAME" | head -10
echo ""
echo "Transfer to your MacBook:"
echo "  scp $PACKAGE_NAME your-mac.local:~/"
echo ""
echo "Then on your MacBook:"
echo "  tar -xzf $PACKAGE_NAME"
echo "  cd rx-benchmark"
echo "  ln -s /path/to/rx-python/bin/rx rx-py  # Create Python binary link"
echo "  ./test/comparison/wiki-benchmark.sh /path/to/wiki.xml"
echo ""
