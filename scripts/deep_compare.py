#!/usr/bin/env python3
"""Deep comparison of Python vs Go rx on core.log with 3 patterns.
Compares outputs field-by-field including offsets and line numbers."""
import json
import os
import subprocess
import sys
import tempfile
import time

GO_RX = '/Users/wlame/dev/_rx2/rx-go/rx'
FILE = '/playground/core.log-2025121008'
# Python CLI takes a single pattern arg — use pipe-separated regex for multi-pattern.
# Go CLI also accepts a single pattern the same way.
PATTERN = 'ERROR|timeout|WARNING'

def run_python(pattern, file_path, out_path, cache_dir):
    """Run Python rx with --json, no cache."""
    args = ['--json', '--no-cache', '--no-index', pattern, file_path]
    code = """
import sys
sys.argv = ['rx'] + sys.argv[1:]
from rx.cli.main import cli
cli(standalone_mode=False)
"""
    env = dict(os.environ)
    env['RX_NO_CACHE'] = '1'
    env['RX_NO_INDEX'] = '1'
    env['RX_CACHE_DIR'] = cache_dir

    start = time.monotonic()
    result = subprocess.run(
        ['python3', '-c', code] + args,
        capture_output=True, env=env, timeout=600,
        cwd='/Users/wlame/dev/_rx2/rx-python'
    )
    elapsed = time.monotonic() - start

    with open(out_path, 'wb') as f:
        f.write(result.stdout)

    if result.stderr:
        stderr_path = out_path.replace('.json', '.stderr')
        with open(stderr_path, 'wb') as f:
            f.write(result.stderr)

    return elapsed

def run_go(pattern, file_path, out_path, cache_dir):
    """Run Go rx with --json, no cache."""
    cmd = [GO_RX, 'trace', '--json', '--no-cache', '--no-index', pattern, file_path]

    env = dict(os.environ)
    env['RX_NO_CACHE'] = '1'
    env['RX_NO_INDEX'] = '1'
    env['RX_CACHE_DIR'] = cache_dir

    start = time.monotonic()
    result = subprocess.run(
        cmd, capture_output=True, env=env, timeout=600
    )
    elapsed = time.monotonic() - start

    with open(out_path, 'wb') as f:
        f.write(result.stdout)

    if result.stderr:
        stderr_path = out_path.replace('.json', '.stderr')
        with open(stderr_path, 'wb') as f:
            f.write(result.stderr)

    return elapsed

def analyze(py_path, go_path):
    """Deep comparison of two JSON outputs."""
    with open(py_path) as f:
        py = json.load(f)
    with open(go_path) as f:
        go = json.load(f)

    print("\n=== STRUCTURE COMPARISON ===")

    # Top-level fields
    for field in ['total_matches', 'total_files']:
        pv = py.get(field)
        gv = go.get(field)
        status = "OK" if pv == gv else f"MISMATCH py={pv} go={gv}"
        print(f"  {field}: py={pv} go={gv} [{status}]")

    py_matches = py.get('matches') or []
    go_matches = go.get('matches') or []
    print(f"  matches array length: py={len(py_matches)} go={len(go_matches)}")

    # Pattern distribution
    print("\n=== PATTERN DISTRIBUTION ===")
    py_by_pat = {}
    go_by_pat = {}
    for m in py_matches:
        p = m.get('pattern', '?')
        py_by_pat[p] = py_by_pat.get(p, 0) + 1
    for m in go_matches:
        p = m.get('pattern', '?')
        go_by_pat[p] = go_by_pat.get(p, 0) + 1

    all_pats = sorted(set(list(py_by_pat.keys()) + list(go_by_pat.keys())))
    for p in all_pats:
        pc = py_by_pat.get(p, 0)
        gc = go_by_pat.get(p, 0)
        pat_name = py.get('patterns', {}).get(p, go.get('patterns', {}).get(p, '?'))
        status = "OK" if pc == gc else "MISMATCH"
        print(f"  {p} ({pat_name}): py={pc} go={gc} [{status}]")

    # File distribution
    print("\n=== FILE DISTRIBUTION ===")
    py_files = py.get('files', {})
    go_files = go.get('files', {})
    print(f"  Python files: {py_files}")
    print(f"  Go files:     {go_files}")

    # Detailed match comparison (first N matches)
    print("\n=== MATCH-BY-MATCH COMPARISON (first 20) ===")
    min_len = min(len(py_matches), len(go_matches), 20)

    mismatches = 0
    for i in range(min_len):
        pm = py_matches[i]
        gm = go_matches[i]

        diffs = []
        for field in ['pattern', 'file', 'offset', 'relative_line_number',
                       'absolute_line_number', 'line_text']:
            pv = pm.get(field)
            gv = gm.get(field)
            if pv != gv:
                # For line_text, show first 50 chars
                if field == 'line_text':
                    pv_show = str(pv)[:50] if pv else 'None'
                    gv_show = str(gv)[:50] if gv else 'None'
                    diffs.append(f"{field}: py={pv_show}... go={gv_show}...")
                else:
                    diffs.append(f"{field}: py={pv} go={gv}")

        if diffs:
            mismatches += 1
            print(f"  [{i}] DIFF: {'; '.join(diffs)}")
        else:
            print(f"  [{i}] OK offset={pm.get('offset')} line={pm.get('relative_line_number')} pat={pm.get('pattern')}")

    # Check remaining matches
    if len(py_matches) > 20 or len(go_matches) > 20:
        print(f"\n=== REMAINING MATCHES (20+) ===")
        remaining_diffs = 0
        offset_diffs = 0
        line_diffs = 0
        text_diffs = 0

        max_check = min(len(py_matches), len(go_matches))
        for i in range(20, max_check):
            pm = py_matches[i]
            gm = go_matches[i]
            if pm.get('offset') != gm.get('offset'):
                offset_diffs += 1
            if pm.get('relative_line_number') != gm.get('relative_line_number'):
                line_diffs += 1
            if pm.get('line_text') != gm.get('line_text'):
                text_diffs += 1
            if pm != gm:
                remaining_diffs += 1

        print(f"  Checked matches 20-{max_check}")
        print(f"  Offset mismatches: {offset_diffs}")
        print(f"  Line number mismatches: {line_diffs}")
        print(f"  Line text mismatches: {text_diffs}")
        print(f"  Total mismatches: {remaining_diffs}")

    # Offset ranges
    if py_matches:
        py_offsets = [m['offset'] for m in py_matches]
        print(f"\n  Python offset range: {min(py_offsets)} - {max(py_offsets)}")
    if go_matches:
        go_offsets = [m['offset'] for m in go_matches]
        print(f"  Go offset range:     {min(go_offsets)} - {max(go_offsets)}")

    # Chunks info
    if py.get('file_chunks'):
        print(f"\n  Python file_chunks: {py['file_chunks']}")
    if go.get('file_chunks'):
        print(f"  Go file_chunks:     {go['file_chunks']}")

def main():
    results_dir = tempfile.mkdtemp(prefix='rx-deep-')
    py_cache = tempfile.mkdtemp(prefix='rx-py-cache-')
    go_cache = tempfile.mkdtemp(prefix='rx-go-cache-')

    print(f"File: {FILE}")
    print(f"Size: {os.path.getsize(FILE) / (1024**3):.1f} GB")
    print(f"Pattern: {PATTERN}")
    print(f"Cache: DISABLED (RX_NO_CACHE=1, RX_NO_INDEX=1)")
    print(f"Limit: NONE (unlimited)")
    print(f"Results dir: {results_dir}")
    print(f"Py cache: {py_cache}")
    print(f"Go cache: {go_cache}")
    print()

    py_out = os.path.join(results_dir, 'python.json')
    go_out = os.path.join(results_dir, 'golang.json')

    print("Running Python rx...")
    sys.stdout.flush()
    py_time = run_python(PATTERN, FILE, py_out, py_cache)
    print(f"  Python time: {py_time:.2f}s")
    print(f"  Python output size: {os.path.getsize(py_out) / 1024:.1f} KB")
    sys.stdout.flush()

    print("\nRunning Go rx...")
    sys.stdout.flush()
    go_time = run_go(PATTERN, FILE, go_out, go_cache)
    print(f"  Go time: {go_time:.2f}s")
    print(f"  Go output size: {os.path.getsize(go_out) / 1024:.1f} KB")
    sys.stdout.flush()

    speedup = py_time / go_time if go_time > 0 else float('inf')
    print(f"\n=== TIMING ===")
    print(f"  Python: {py_time:.2f}s")
    print(f"  Go:     {go_time:.2f}s")
    print(f"  Speedup: {speedup:.1f}x {'(Go faster)' if speedup > 1 else '(Python faster)'}")

    # Verify caches are empty (we disabled them)
    py_cache_files = []
    go_cache_files = []
    for root, dirs, files in os.walk(py_cache):
        py_cache_files.extend(files)
    for root, dirs, files in os.walk(go_cache):
        go_cache_files.extend(files)
    print(f"\n=== CACHE VERIFICATION ===")
    print(f"  Python cache files: {len(py_cache_files)} (should be 0)")
    print(f"  Go cache files: {len(go_cache_files)} (should be 0)")

    analyze(py_out, go_out)

    print(f"\nFull outputs saved to: {results_dir}")

if __name__ == '__main__':
    main()
