#!/usr/bin/env bash
set -euo pipefail

# Python Parity Testing Script
# Compares Go and Python implementations of rx
#
# IMPORTANT: Tests run with --no-cache and --no-index flags
# to ensure pure comparison without caching effects

# Paths
PYTHON_RX="/home/wlame/rx/rx-python/.venv/bin/rx"
GO_RX="/home/wlame/rx/rx-go-new/bin/rx"
TEST_DATA="/home/wlame/rx/rx-go-new/test/integration/testdata"
RESULTS_DIR="/home/wlame/rx/rx-go-new/.claude/parity-results"

# Cache directories (separate to avoid conflicts, but --no-cache disables them)
export RX_CACHE_DIR_PY="/tmp/rx-parity-cache-python"
export RX_CACHE_DIR_GO="/tmp/rx-parity-cache-go"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Cleanup and setup
cleanup() {
    rm -rf "$RX_CACHE_DIR_PY" "$RX_CACHE_DIR_GO" 2>/dev/null || true
    mkdir -p "$RESULTS_DIR"
}

# Run a test scenario
run_test() {
    local test_name="$1"
    local description="$2"
    shift 2
    local args=("$@")

    echo ""
    echo "========================================="
    echo "Test: $test_name"
    echo "Description: $description"
    echo "Python: rx ${args[*]}"
    echo "Go:     rx trace ${args[*]}"
    echo "========================================="

    # Prepare output files
    local py_out="$RESULTS_DIR/${test_name}_python.json"
    local go_out="$RESULTS_DIR/${test_name}_go.json"
    local diff_out="$RESULTS_DIR/${test_name}_diff.txt"

    # Run Python version (use env vars to disable cache/index)
    echo -n "Running Python rx... "
    if RX_CACHE_DIR="$RX_CACHE_DIR_PY" RX_NO_CACHE=1 RX_NO_INDEX=1 "$PYTHON_RX" "${args[@]}" --json > "$py_out" 2>&1; then
        echo -e "${GREEN}OK${NC}"
        py_matches=$(jq -r '.total_matches // .matches | length' "$py_out" 2>/dev/null || echo "0")
        echo "  Python: $py_matches matches"
    else
        echo -e "${RED}FAILED${NC}"
        cat "$py_out" | head -5
        py_matches="ERROR"
    fi

    # Run Go version (use flags to disable cache/index)
    echo -n "Running Go rx... "
    if RX_CACHE_DIR="$RX_CACHE_DIR_GO" "$GO_RX" trace "${args[@]}" --json --no-cache --no-index > "$go_out" 2>&1; then
        echo -e "${GREEN}OK${NC}"
        go_matches=$(jq -r '.total_matches // .matches | length' "$go_out" 2>/dev/null || echo "0")
        echo "  Go: $go_matches matches"
    else
        echo -e "${RED}FAILED${NC}"
        cat "$go_out" | head -5
        go_matches="ERROR"
    fi

    # Compare results
    if [ "$py_matches" = "$go_matches" ] && [ "$py_matches" != "ERROR" ]; then
        echo -e "${GREEN}✓ PASS${NC} - Match counts identical: $py_matches"

        # Detailed comparison
        if compare_json "$py_out" "$go_out" > "$diff_out" 2>&1; then
            echo -e "${GREEN}✓ PASS${NC} - Response structures match"
        else
            echo -e "${YELLOW}⚠ WARNING${NC} - Match counts identical but structures differ"
            echo "  See: $diff_out"
        fi
    else
        echo -e "${RED}✗ FAIL${NC} - Match counts differ (Python: $py_matches, Go: $go_matches)"
        echo "  Python output: $py_out"
        echo "  Go output: $go_out"
    fi
}

# Compare JSON responses
compare_json() {
    local py_json="$1"
    local go_json="$2"

    # Extract key fields for comparison
    local fields=("total_matches" "request_id" "scanned_files" "skipped_files")

    for field in "${fields[@]}"; do
        local py_val=$(jq -r ".$field" "$py_json" 2>/dev/null || echo "null")
        local go_val=$(jq -r ".$field" "$go_json" 2>/dev/null || echo "null")

        if [ "$field" = "request_id" ]; then
            # Skip request_id (will be different)
            continue
        fi

        if [ "$py_val" != "$go_val" ]; then
            echo "Field '$field' differs:"
            echo "  Python: $py_val"
            echo "  Go: $go_val"
            return 1
        fi
    done

    return 0
}

# Main test suite
main() {
    echo "================================================"
    echo "RX Python vs Go Parity Testing"
    echo "================================================"
    echo "Python RX: $PYTHON_RX"
    echo "Go RX: $GO_RX"
    echo "Test Data: $TEST_DATA"
    echo ""

    # Verify binaries exist
    if [ ! -f "$PYTHON_RX" ]; then
        echo -e "${RED}ERROR: Python rx not found at $PYTHON_RX${NC}"
        exit 1
    fi

    if [ ! -f "$GO_RX" ]; then
        echo -e "${RED}ERROR: Go rx not found at $GO_RX${NC}"
        echo "Build it with: go build -o bin/rx ./cmd/rx"
        exit 1
    fi

    # Verify jq is installed
    if ! command -v jq &> /dev/null; then
        echo -e "${YELLOW}WARNING: jq not installed, JSON comparison will be limited${NC}"
    fi

    cleanup

    # Test 1: Basic search
    run_test "basic_search" \
        "Basic ERROR pattern search in small file" \
        "ERROR" "$TEST_DATA/app.log"

    # Test 2: Multiple patterns
    run_test "multi_pattern" \
        "Multiple pattern search (ERROR, WARNING, CRITICAL)" \
        "-e" "ERROR" "-e" "WARNING" "-e" "CRITICAL" "$TEST_DATA/app.log"

    # Test 3: Context extraction
    run_test "context_extraction" \
        "Search with context (before=2, after=2)" \
        "-C" "2" "ERROR" "$TEST_DATA/app.log"

    # Test 4: Case insensitive
    run_test "case_insensitive" \
        "Case insensitive search" \
        "-i" "error" "$TEST_DATA/app.log"

    # Test 5: Compressed file (gzip)
    run_test "compressed_gzip" \
        "Search in gzip compressed file" \
        "ERROR" "$TEST_DATA/app.log.gz"

    # Test 6: Compressed file (bzip2)
    run_test "compressed_bzip2" \
        "Search in bzip2 compressed file" \
        "ERROR" "$TEST_DATA/app.log.bz2"

    # Test 7: Directory scan
    run_test "directory_scan" \
        "Scan entire test directory" \
        "ERROR" "$TEST_DATA"

    # Test 8: Large file
    run_test "large_file" \
        "Search in large file (84MB)" \
        "ERROR" "$TEST_DATA/large.log"

    # Test 9: Max results
    run_test "max_results" \
        "Search with max results limit" \
        "--max-results=100" "ERROR" "$TEST_DATA/large.log"

    # Test 10: Before context only
    run_test "before_context" \
        "Search with before context only" \
        "-B" "3" "ERROR" "$TEST_DATA/app.log"

    # Test 11: After context only
    run_test "after_context" \
        "Search with after context only" \
        "-A" "3" "ERROR" "$TEST_DATA/app.log"

    # Summary
    echo ""
    echo "================================================"
    echo "Parity Testing Complete"
    echo "================================================"
    echo "Results saved to: $RESULTS_DIR"
    echo ""
    echo "To review differences:"
    echo "  ls -lh $RESULTS_DIR"
    echo "  cat $RESULTS_DIR/*_diff.txt"
}

main "$@"
