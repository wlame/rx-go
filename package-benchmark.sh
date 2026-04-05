#!/usr/bin/env bash
set -euo pipefail

PACKAGE_NAME="rx-benchmark-extended-fixed"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
PACKAGE_DIR="/tmp/${PACKAGE_NAME}"
TARBALL="${PACKAGE_NAME}.tar.gz"

echo "Creating benchmark package with deadlock fix..."
echo ""

# Clean and create package directory
rm -rf "$PACKAGE_DIR"
mkdir -p "$PACKAGE_DIR"

# Copy ARM64 binary
echo "→ Copying ARM64 binary..."
mkdir -p "$PACKAGE_DIR/dist"
cp dist/rx-darwin-arm64 "$PACKAGE_DIR/dist/"
chmod +x "$PACKAGE_DIR/dist/rx-darwin-arm64"

# Copy test scripts
echo "→ Copying test scripts..."
mkdir -p "$PACKAGE_DIR/test/comparison"
cp test/comparison/wiki-benchmark-extended.sh "$PACKAGE_DIR/test/comparison/"
cp test/comparison/wiki-benchmark-resume.sh "$PACKAGE_DIR/test/comparison/"
chmod +x "$PACKAGE_DIR/test/comparison/"*.sh

# Copy documentation
echo "→ Copying documentation..."
cp EXTENDED_BENCHMARK_READY.md "$PACKAGE_DIR/"
cp BENCHMARK_FIX.md "$PACKAGE_DIR/"

# Create package info
cat > "$PACKAGE_DIR/PACKAGE_INFO.md" <<EOF
# RX Benchmark Package - Deadlock Fix

**Built:** ${TIMESTAMP}
**Fix:** Batching result collector for high-throughput multi-pattern searches

## What's Fixed

The previous version had a deadlock/hang issue when running multi-pattern searches with no limits on large files (75GB Wikipedia). The problem:

- \`result_collector.go\` held mutex for EVERY match
- Heavy work under lock: map lookups, fmt.Sprintf(), slice append
- Channel buffer (200) filled quickly with thousands of matches
- Workers blocked, collector slow → system ground to halt

## Solution

New \`result_collector_fixed.go\` with batching:
- Buffers matches (minimal lock time)
- Processes in batches of 100 (main lock held only during batch)
- Should handle thousands of matches efficiently

## Files Included

- \`dist/rx-darwin-arm64\` - ARM64 macOS binary with fix
- \`test/comparison/wiki-benchmark-extended.sh\` - Main benchmark (78+ tests)
- \`test/comparison/wiki-benchmark-resume.sh\` - Resume from Suite 4
- \`EXTENDED_BENCHMARK_READY.md\` - Full benchmark guide
- \`BENCHMARK_FIX.md\` - Detailed fix explanation

## Quick Start

\`\`\`bash
# Extract
tar -xzf ${TARBALL}
cd ${PACKAGE_NAME}

# Link Python binary
ln -s /path/to/rx-python/bin/rx rx-py

# Verify
./dist/rx-darwin-arm64 --version
./rx-py --version

# Run benchmark
./test/comparison/wiki-benchmark-extended.sh /path/to/wiki.xml
\`\`\`

See EXTENDED_BENCHMARK_READY.md for full details.
EOF

# Create tarball
echo "→ Creating tarball..."
cd /tmp
tar -czf "$TARBALL" "$PACKAGE_NAME"
mv "$TARBALL" /home/wlame/rx/rx-go-new/

# Cleanup
rm -rf "$PACKAGE_DIR"

echo ""
echo "✅ Package created: ${TARBALL}"
echo ""
echo "Size: $(du -h /home/wlame/rx/rx-go-new/${TARBALL} | cut -f1)"
echo ""
echo "Transfer to MacBook:"
echo "  scp ${TARBALL} mac.local:~/"
echo ""
