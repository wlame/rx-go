#!/usr/bin/env python3
"""Compare Python and Go rx implementations on /playground/ files."""
import json
import os
import subprocess
import sys
import tempfile
import time

GO_RX = '/Users/wlame/dev/_rx2/rx-go/rx'
RESULTS_DIR = tempfile.mkdtemp(prefix='rx-comparison-')

def human_size(path):
    """Return human-readable file size."""
    size = os.path.getsize(path)
    for unit in ['B', 'KB', 'MB', 'GB']:
        if size < 1024:
            return f'{size:.1f}{unit}'
        size /= 1024
    return f'{size:.1f}TB'

def run_python(pattern, file_path, extra_args=None, cache_dir=None):
    """Run Python rx and return (time_seconds, json_output_path)."""
    out_path = os.path.join(RESULTS_DIR, f'py_{int(time.time()*1000)}.json')
    env = dict(os.environ)
    env['RX_NO_CACHE'] = '1'
    env['RX_NO_INDEX'] = '1'
    if cache_dir:
        env['RX_CACHE_DIR'] = cache_dir

    args = ['--json', pattern, file_path]
    if extra_args:
        args.extend(extra_args)

    code = f"""
import sys
sys.argv = ['rx'] + sys.argv[1:]
from rx.cli.main import cli
cli(standalone_mode=False)
"""
    start = time.monotonic()
    try:
        result = subprocess.run(
            ['python3', '-c', code] + args,
            capture_output=True, env=env, timeout=300, cwd='/Users/wlame/dev/_rx2/rx-python'
        )
        with open(out_path, 'wb') as f:
            f.write(result.stdout)
    except subprocess.TimeoutExpired:
        with open(out_path, 'w') as f:
            json.dump({'error': 'timeout'}, f)
    elapsed = time.monotonic() - start
    return elapsed, out_path

def run_go(pattern, file_path, extra_args=None, cache_dir=None):
    """Run Go rx and return (time_seconds, json_output_path)."""
    out_path = os.path.join(RESULTS_DIR, f'go_{int(time.time()*1000)}.json')
    env = dict(os.environ)
    env['RX_NO_CACHE'] = '1'
    env['RX_NO_INDEX'] = '1'
    if cache_dir:
        env['RX_CACHE_DIR'] = cache_dir

    cmd = [GO_RX, 'trace', '--json', pattern, file_path]
    if extra_args:
        cmd.extend(extra_args)

    start = time.monotonic()
    try:
        result = subprocess.run(cmd, capture_output=True, env=env, timeout=300)
        with open(out_path, 'wb') as f:
            f.write(result.stdout)
    except subprocess.TimeoutExpired:
        with open(out_path, 'w') as f:
            json.dump({'error': 'timeout'}, f)
    elapsed = time.monotonic() - start
    return elapsed, out_path

def load_result(path):
    """Load JSON result, return dict or None."""
    try:
        with open(path) as f:
            return json.load(f)
    except Exception:
        return None

def compare_results(py_data, go_data):
    """Compare two result dicts. Return (status, details)."""
    if py_data is None and go_data is None:
        return 'ERR', 'both failed to produce JSON'
    if py_data is None:
        return 'ERR', 'Python failed'
    if go_data is None:
        return 'ERR', 'Go failed'

    py_matches = py_data.get('matches') or []
    go_matches = go_data.get('matches') or []
    py_count = len(py_matches)
    go_count = len(go_matches)

    py_files = py_data.get('total_files') or len(py_data.get('scanned_files') or [])
    go_files = go_data.get('total_files') or len(go_data.get('scanned_files') or [])

    if py_count == go_count:
        return 'PASS', ''
    else:
        return 'DIFF', f'py={py_count} go={go_count} (files: py={py_files} go={go_files})'

# Test definitions: (label, file, pattern, extra_args)
TESTS = [
    # Small file
    ('small.txt / yahoo', '/playground/small.txt', 'yahoo', None),
    ('small.txt / hotmail', '/playground/small.txt', 'hotmail', None),

    # Medium file (11MB)
    ('SOMELOG 11M / AudioDevice', '/playground/SOMELOG.log', 'AudioDevice', None),
    ('SOMELOG 11M / timestamp', '/playground/SOMELOG.log', r'\d{4}-\d{1,2}-\d{1,2}', None),

    # Large text (465MB)
    ('middleware 465M / ERROR', '/playground/middleware.log-2025121008', 'ERROR', None),
    ('middleware 465M / error', '/playground/middleware.log-2025121008', 'error', None),

    # Large text (572MB)
    ('postgresql 572M / LOG:', '/playground/postgresql.log-2025121008', 'LOG:', None),
    ('postgresql 572M / duration', '/playground/postgresql.log-2025121008', 'duration', None),

    # Very large with limit (6.3GB)
    ('core.log 6.3G / ERROR max50', '/playground/core.log-2025121008', 'ERROR', ['--max-results', '50']),

    # Very large with limit (7.7GB)
    ('logs_stat 7.7G / @hotmail max50', '/playground/logs_stat.txt', '@hotmail', ['--max-results', '50']),

    # Gzip (293MB compressed, ~572MB decompressed)
    ('postgresql.gz 293M / LOG:', '/playground/postgresql.log-2025121008.gz', 'LOG:', None),

    # Gzip (113MB compressed)
    ('middleware.gz 113M / ERROR', '/playground/middleware.log-2025121008.gz', 'ERROR', None),

    # Zstd (53MB compressed)
    ('postgresql.zst 53M / duration', '/playground/postgresql.log-2025121008.zst', 'duration', None),

    # Multi-pattern
    ('middleware 465M / multi', '/playground/middleware.log-2025121008', 'ERROR|error|warn', None),
]

def main():
    print(f'Results: {RESULTS_DIR}')
    print()

    # Header
    fmt = '%-40s %8s %8s %8s %8s %14s %s'
    print(fmt % ('Test Case', 'Size', 'Python', 'Go', 'Speedup', 'Matches(Py/Go)', 'Status'))
    print(fmt % ('-' * 40, '-' * 8, '-' * 8, '-' * 8, '-' * 8, '-' * 14, '-' * 6))

    total_pass = 0
    total_diff = 0
    total_err = 0
    diffs = []

    for label, file_path, pattern, extra_args in TESTS:
        size = human_size(file_path) if os.path.exists(file_path) else 'N/A'

        py_cache = tempfile.mkdtemp(prefix='rx-py-')
        go_cache = tempfile.mkdtemp(prefix='rx-go-')

        py_time, py_path = run_python(pattern, file_path, extra_args, py_cache)
        go_time, go_path = run_go(pattern, file_path, extra_args, go_cache)

        py_data = load_result(py_path)
        go_data = load_result(go_path)

        py_count = len((py_data or {}).get('matches') or [])
        go_count = len((go_data or {}).get('matches') or [])

        status, detail = compare_results(py_data, go_data)

        speedup = f'{py_time / go_time:.1f}x' if go_time > 0 else 'N/A'

        match_str = f'{py_count}/{go_count}'
        status_str = status
        if status == 'DIFF':
            status_str = f'DIFF'
            diffs.append((label, detail))

        if status == 'PASS':
            total_pass += 1
        elif status == 'DIFF':
            total_diff += 1
        else:
            total_err += 1

        print(fmt % (label, size, f'{py_time:.2f}s', f'{go_time:.2f}s', speedup, match_str, status_str))

    print()
    print(f'Summary: {total_pass} PASS, {total_diff} DIFF, {total_err} ERR out of {len(TESTS)} tests')

    if diffs:
        print()
        print('=== DIFFERENCES ===')
        for label, detail in diffs:
            print(f'  {label}: {detail}')

    print(f'\nFull results in: {RESULTS_DIR}')

if __name__ == '__main__':
    main()
