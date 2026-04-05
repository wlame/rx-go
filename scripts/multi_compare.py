#!/usr/bin/env python3
"""Compare Python vs Go rx on directory + multiple files with different patterns."""
import json
import os
import subprocess
import sys
import tempfile
import time

GO_RX = '/Users/wlame/dev/_rx2/rx-go/rx'
RESULTS_DIR = tempfile.mkdtemp(prefix='rx-multi-')

def run_rx(impl, pattern, paths, out_path, cache_dir):
    """Run rx (python or go) and return elapsed seconds."""
    env = dict(os.environ)
    env['RX_NO_CACHE'] = '1'
    env['RX_NO_INDEX'] = '1'
    env['RX_CACHE_DIR'] = cache_dir

    if impl == 'python':
        code = "import sys; sys.argv = ['rx'] + sys.argv[1:]; from rx.cli.main import cli; cli(standalone_mode=False)"
        cmd = ['python3', '-c', code, '--json', '--no-cache', '--no-index', pattern] + paths
        cwd = '/Users/wlame/dev/_rx2/rx-python'
    else:
        cmd = [GO_RX, 'trace', '--json', '--no-cache', '--no-index', pattern] + paths
        cwd = None

    start = time.monotonic()
    try:
        result = subprocess.run(cmd, capture_output=True, env=env, timeout=600, cwd=cwd)
        with open(out_path, 'wb') as f:
            f.write(result.stdout)
        if result.stderr:
            with open(out_path + '.stderr', 'wb') as f:
                f.write(result.stderr)
    except subprocess.TimeoutExpired:
        with open(out_path, 'w') as f:
            json.dump({'_error': 'timeout'}, f)
    return time.monotonic() - start

def load_json(path):
    try:
        with open(path) as f:
            return json.load(f)
    except:
        return None

def compare(label, pattern, paths):
    """Run both, compare, print results."""
    py_cache = tempfile.mkdtemp(prefix='rx-py-')
    go_cache = tempfile.mkdtemp(prefix='rx-go-')
    py_out = os.path.join(RESULTS_DIR, f'py_{label}.json')
    go_out = os.path.join(RESULTS_DIR, f'go_{label}.json')

    sys.stdout.write(f'\n{"="*70}\n{label}\n  pattern: {pattern}\n  paths: {paths}\n')
    sys.stdout.flush()

    # Python
    sys.stdout.write('  Running Python... ')
    sys.stdout.flush()
    py_time = run_rx('python', pattern, paths, py_out, py_cache)
    py_data = load_json(py_out)
    py_matches = py_data.get('matches') or [] if py_data else []
    sys.stdout.write(f'{py_time:.2f}s, {len(py_matches)} matches\n')
    sys.stdout.flush()

    # Go
    sys.stdout.write('  Running Go...     ')
    sys.stdout.flush()
    go_time = run_rx('go', pattern, paths, go_out, go_cache)
    go_data = load_json(go_out)
    go_matches = go_data.get('matches') or [] if go_data else []
    sys.stdout.write(f'{go_time:.2f}s, {len(go_matches)} matches\n')
    sys.stdout.flush()

    # Timing
    speedup = py_time / go_time if go_time > 0 else 0
    print(f'  Speedup: {speedup:.1f}x {"(Go faster)" if speedup > 1 else "(Python faster)"}')

    if py_data is None or go_data is None:
        print(f'  ERROR: {"Python" if py_data is None else "Go"} produced no JSON')
        return label, len(py_matches), len(go_matches), py_time, go_time, 'ERR'

    # Structure comparison
    py_keys = set(py_data.keys())
    go_keys = set(go_data.keys())
    volatile = {'time', 'request_id', 'cli_command'}
    missing = py_keys - go_keys - volatile
    if missing:
        print(f'  MISSING keys from Go: {missing}')

    # Match count
    print(f'  Match count: py={len(py_matches)} go={len(go_matches)}', end='')
    if len(py_matches) == len(go_matches):
        print(' [OK]')
    else:
        print(f' [DIFF by {abs(len(py_matches) - len(go_matches))}]')

    # File distribution
    py_files = py_data.get('files', {})
    go_files = go_data.get('files', {})
    print(f'  Files: py={len(py_files)} go={len(go_files)}')
    if py_files:
        for fid, fp in sorted(py_files.items()):
            gp = go_files.get(fid, 'MISSING')
            eq = 'OK' if fp == gp else 'DIFF'
            print(f'    {fid}: {os.path.basename(fp)} [{eq}]')

    # Scanned/skipped files
    py_scanned = py_data.get('scanned_files') or []
    go_scanned = go_data.get('scanned_files') or []
    py_skipped = py_data.get('skipped_files') or []
    go_skipped = go_data.get('skipped_files') or []
    print(f'  Scanned: py={len(py_scanned)} go={len(go_scanned)}')
    print(f'  Skipped: py={len(py_skipped)} go={len(go_skipped)}')

    # Compare offsets for first 20 matches
    min_check = min(len(py_matches), len(go_matches), 20)
    offset_ok = 0
    offset_diff = 0
    line_ok = 0
    line_diff = 0
    text_ok = 0
    text_diff = 0
    for i in range(min_check):
        pm, gm = py_matches[i], go_matches[i]
        if pm.get('offset') == gm.get('offset'):
            offset_ok += 1
        else:
            offset_diff += 1
        if pm.get('relative_line_number') == gm.get('relative_line_number'):
            line_ok += 1
        else:
            line_diff += 1
        if pm.get('line_text') == gm.get('line_text'):
            text_ok += 1
        else:
            text_diff += 1

    print(f'  First {min_check} matches: offsets={offset_ok}ok/{offset_diff}diff, '
          f'lines={line_ok}ok/{line_diff}diff, text={text_ok}ok/{text_diff}diff')

    # Check ALL matches for offset/text
    if len(py_matches) == len(go_matches) and len(py_matches) > 20:
        all_offset_diff = sum(1 for i in range(len(py_matches))
                             if py_matches[i].get('offset') != go_matches[i].get('offset'))
        all_text_diff = sum(1 for i in range(len(py_matches))
                           if py_matches[i].get('line_text') != go_matches[i].get('line_text'))
        print(f'  ALL {len(py_matches)} matches: offset_diffs={all_offset_diff}, text_diffs={all_text_diff}')

    # Pattern attribution
    py_by_pat = {}
    go_by_pat = {}
    for m in py_matches:
        p = m.get('pattern', '?')
        py_by_pat[p] = py_by_pat.get(p, 0) + 1
    for m in go_matches:
        p = m.get('pattern', '?')
        go_by_pat[p] = go_by_pat.get(p, 0) + 1

    pats = py_data.get('patterns', {})
    if len(pats) > 1:
        print(f'  Pattern distribution:')
        for pid in sorted(set(list(py_by_pat.keys()) + list(go_by_pat.keys()))):
            pc = py_by_pat.get(pid, 0)
            gc = go_by_pat.get(pid, 0)
            name = pats.get(pid, '?')
            eq = 'OK' if pc == gc else 'DIFF'
            print(f'    {pid} ({name}): py={pc} go={gc} [{eq}]')

    status = 'PASS' if len(py_matches) == len(go_matches) else 'DIFF'
    return label, len(py_matches), len(go_matches), py_time, go_time, status

def main():
    print(f'Results dir: {RESULTS_DIR}')
    print(f'Cache: DISABLED, Index: DISABLED')
    results = []

    # Test 1: Directory search (twitt/ has text files + compressed)
    results.append(compare(
        'twitt_dir_hotmail',
        '@hotmail',
        ['/playground/twitt/']
    ))

    # Test 2: Multiple separate files
    results.append(compare(
        'multi_files_error',
        'error|ERROR|Error',
        ['/playground/stdout-long-lines-analysis', '/playground/SOMELOG.log']
    ))

    # Test 3: Directory + file combined
    results.append(compare(
        'dir_plus_file',
        '@yahoo',
        ['/playground/twitt/twitters.txt', '/playground/small.txt']
    ))

    # Test 4: Large file from logs_stat (7.7GB) — specific pattern
    results.append(compare(
        'logs_stat_hotmail',
        '@hotmail.com',
        ['/playground/logs_stat.txt']
    ))

    # Test 5: stdout-long-lines (2.2GB) — WARNING pattern
    results.append(compare(
        'stdout_WARN',
        'WARN',
        ['/playground/stdout-long-lines-analysis']
    ))

    # Test 6: Multiple patterns via alternation on stdout-long-lines
    results.append(compare(
        'stdout_multi_pattern',
        'ERROR|WARN|Processing failure',
        ['/playground/stdout-long-lines-analysis']
    ))

    # Summary table
    print(f'\n{"="*70}')
    print('SUMMARY')
    print(f'{"="*70}')
    fmt = '%-30s %10s %10s %8s %8s %8s %s'
    print(fmt % ('Test', 'Py Match', 'Go Match', 'Py Time', 'Go Time', 'Speedup', 'Status'))
    print(fmt % ('-'*30, '-'*10, '-'*10, '-'*8, '-'*8, '-'*8, '-'*6))

    for label, py_m, go_m, py_t, go_t, status in results:
        spd = f'{py_t/go_t:.1f}x' if go_t > 0 else 'N/A'
        print(fmt % (label, py_m, go_m, f'{py_t:.1f}s', f'{go_t:.1f}s', spd, status))

    print(f'\nFull JSON outputs: {RESULTS_DIR}')

if __name__ == '__main__':
    main()
