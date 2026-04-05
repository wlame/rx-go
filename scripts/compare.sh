#!/usr/bin/env bash
# compare.sh — Run both Python and Go rx on /playground/ files and compare results
set -uo pipefail

GO_RX="/Users/wlame/dev/_rx2/rx-go/rx"
RESULTS_DIR=$(mktemp -d /tmp/rx-comparison.XXXXXX)
PY_CACHE=$(mktemp -d /tmp/rx-py-cache.XXXXXX)
GO_CACHE=$(mktemp -d /tmp/rx-go-cache.XXXXXX)

cleanup() { rm -rf "$PY_CACHE" "$GO_CACHE"; }
trap cleanup EXIT

echo "Results dir: $RESULTS_DIR"
echo ""

printf "%-45s %10s %10s %10s %10s %12s %s\n" "Test Case" "Size" "Py Time" "Go Time" "Speedup" "Matches(Py/Go)" "Status"
printf "%-45s %10s %10s %10s %10s %12s %s\n" "---------------------------------------------" "----------" "----------" "----------" "----------" "------------" "------"

run_py() {
    local pattern="$1" file="$2" outfile="$3"
    shift 3
    local extra=("$@")
    RX_CACHE_DIR="$PY_CACHE" RX_NO_CACHE=1 python3 -c "
import sys
sys.argv = ['rx', '--json', sys.argv[1], sys.argv[2]] + sys.argv[3:]
from rx.cli.main import cli
cli(standalone_mode=False)
" "$pattern" "$file" "${extra[@]+"${extra[@]}"}" > "$outfile" 2>/dev/null
}

run_go() {
    local pattern="$1" file="$2" outfile="$3"
    shift 3
    RX_CACHE_DIR="$GO_CACHE" RX_NO_CACHE=1 "$GO_RX" trace --json "$pattern" "$file" "$@" > "$outfile" 2>/dev/null
}

now_ms() {
    python3 -c "import time; print(int(time.time()*1000))"
}

run_test() {
    local label="$1"
    local file="$2"
    local pattern="$3"
    shift 3
    local extra_args=("$@")

    # Safe label for filenames (replace / with _)
    local safe_label="${label//\//_}"

    local file_size
    file_size=$(du -h "$file" 2>/dev/null | cut -f1 | tr -d ' ')

    local py_json="$RESULTS_DIR/py_${safe_label}.json"
    local go_json="$RESULTS_DIR/go_${safe_label}.json"

    # Python run with timing
    local py_start py_end py_ms
    py_start=$(now_ms)
    run_py "$pattern" "$file" "$py_json" "${extra_args[@]+"${extra_args[@]}"}" || true
    py_end=$(now_ms)
    py_ms=$((py_end - py_start))

    # Go run with timing
    local go_start go_end go_ms
    go_start=$(now_ms)
    run_go "$pattern" "$file" "$go_json" "${extra_args[@]+"${extra_args[@]}"}" || true
    go_end=$(now_ms)
    go_ms=$((go_end - go_start))

    # Format times
    local py_time go_time
    py_time=$(python3 -c "print(f'{$py_ms/1000:.2f}s')")
    go_time=$(python3 -c "print(f'{$go_ms/1000:.2f}s')")

    # Extract match counts
    local py_matches go_matches
    py_matches=$(python3 -c "import json; d=json.load(open('$py_json')); print(d.get('total_matches', 'N/A'))" 2>/dev/null || echo "ERR")
    go_matches=$(python3 -c "import json; d=json.load(open('$go_json')); print(d.get('total_matches', 'N/A'))" 2>/dev/null || echo "ERR")

    # Status
    local status
    if [ "$py_matches" = "$go_matches" ]; then
        status="PASS"
    elif [ "$py_matches" = "ERR" ] || [ "$go_matches" = "ERR" ]; then
        status="ERR"
    else
        status="DIFF"
        python3 -c "
import json
py = json.load(open('$py_json'))
go = json.load(open('$go_json'))
print(f'total_matches: py={py.get(\"total_matches\")} go={go.get(\"total_matches\")}')
print(f'total_files: py={py.get(\"total_files\")} go={go.get(\"total_files\")}')
print(f'matches_len: py={len(py.get(\"matches\",[]))} go={len(go.get(\"matches\",[]))}')
" > "$RESULTS_DIR/diff_${safe_label}.txt" 2>/dev/null || true
    fi

    # Speedup
    local speedup="N/A"
    if [ "$go_ms" -gt 0 ]; then
        speedup=$(python3 -c "s=$py_ms/$go_ms; print(f'{s:.1f}x')")
    fi

    printf "%-45s %10s %10s %10s %10s %12s %s\n" "$label" "$file_size" "$py_time" "$go_time" "$speedup" "$py_matches/$go_matches" "$status"
}

# Build Go binary
echo "Building Go binary..."
cd /Users/wlame/dev/_rx2/rx-go && go build -o rx ./cmd/rx
echo ""

# --- Small ---
run_test "small.txt / ERROR" "/playground/small.txt" "ERROR"

# --- Medium (11MB) ---
run_test "SOMELOG.log / ERROR" "/playground/SOMELOG.log" "ERROR"
run_test "SOMELOG.log / warning|error" "/playground/SOMELOG.log" "warning|error"
run_test "SOMELOG.log / timestamp" "/playground/SOMELOG.log" '\d{4}-\d{2}-\d{2}'

# --- Large text (465MB-572MB) ---
run_test "postgresql.log / ERROR" "/playground/postgresql.log-2025121008" "ERROR"
run_test "postgresql.log / FATAL" "/playground/postgresql.log-2025121008" "FATAL"
run_test "middleware.log / ERROR" "/playground/middleware.log-2025121008" "ERROR"
run_test "middleware.log / error|warn" "/playground/middleware.log-2025121008" "error|warn"

# --- Very large with limit (6.3GB-7.7GB) ---
run_test "core.log 6.3G / ERROR max100" "/playground/core.log-2025121008" "ERROR" "--max-results" "100"
run_test "logs_stat 7.7G / ERROR max100" "/playground/logs_stat.txt" "ERROR" "--max-results" "100"

# --- Gzip compressed ---
run_test "postgresql.gz / ERROR" "/playground/postgresql.log-2025121008.gz" "ERROR"
run_test "middleware.gz / ERROR" "/playground/middleware.log-2025121008.gz" "ERROR"

# --- Zstd compressed ---
run_test "postgresql.zst / ERROR" "/playground/postgresql.log-2025121008.zst" "ERROR"

# --- Multi-pattern ---
run_test "postgresql.log / multi-pattern" "/playground/postgresql.log-2025121008" "ERROR|WARNING|FATAL"

echo ""
echo "Results saved to: $RESULTS_DIR"
echo ""

# Show any DIFFs
has_diffs=0
for f in "$RESULTS_DIR"/diff_*.txt; do
    [ -f "$f" ] || continue
    if [ -s "$f" ]; then
        has_diffs=1
        echo "=== DIFF: $(basename "$f" .txt) ==="
        cat "$f"
        echo ""
    fi
done

if [ "$has_diffs" -eq 0 ]; then
    echo "All match counts agree between Python and Go."
fi
