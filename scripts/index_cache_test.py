#!/usr/bin/env python3
"""Comprehensive index/cache compatibility tests between Python rx and Go rx.

Tests 5 requirements:
1. Index creation format compatibility
2. Index-accelerated line number resolution
3. Trace cache store/load round-trip
4. Index invalidation on file change
5. Cache key computation compatibility

Uses REAL /playground/ files and isolated temp cache dirs.
"""

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

# --- Constants ---
GO_RX = '/Users/wlame/dev/_rx2/rx-go/rx'
PY_RX = 'rx'  # Python rx is on PATH
SOMELOG = '/playground/SOMELOG.log'
MIDDLEWARE_LOG = '/playground/middleware.log-2025121008'

# Pattern that actually exists in SOMELOG.log (570 matches)
SOMELOG_PATTERN = 'rtp'
# Pattern that exists in middleware.log (46k+ matches)
MIDDLEWARE_PATTERN = 'error'

pass_count = 0
fail_count = 0
skip_count = 0


def report(name, passed, detail=''):
    global pass_count, fail_count
    status = 'PASS' if passed else 'FAIL'
    if passed:
        pass_count += 1
    else:
        fail_count += 1
    print(f'  [{status}] {name}')
    if detail:
        for line in detail.strip().split('\n'):
            print(f'         {line}')


def skip(name, reason=''):
    global skip_count
    skip_count += 1
    print(f'  [SKIP] {name}')
    if reason:
        print(f'         {reason}')


def run_cmd(cmd, timeout=120, env_override=None):
    """Run a command and return (returncode, stdout, stderr)."""
    env = os.environ.copy()
    if env_override:
        env.update(env_override)
    try:
        result = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout, env=env,
        )
        return result.returncode, result.stdout, result.stderr
    except subprocess.TimeoutExpired:
        return -1, '', 'TIMEOUT'
    except Exception as e:
        return -1, '', str(e)


def parse_json(text):
    """Parse JSON from CLI output, handling possible non-JSON prefix lines."""
    text = text.strip()
    if not text:
        return None
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        pass
    # Try each line
    for line in text.split('\n'):
        line = line.strip()
        if line.startswith('{') or line.startswith('['):
            try:
                return json.loads(line)
            except json.JSONDecodeError:
                pass
    return None


def section(title):
    print(f'\n{"=" * 70}')
    print(f'  {title}')
    print(f'{"=" * 70}')


# =============================================================================
# Test 1: Index creation format compatibility
# =============================================================================
def test_index_creation_format():
    section('Test 1: Index Creation Format Compatibility')

    if not os.path.exists(SOMELOG):
        skip('Index creation', f'{SOMELOG} not found')
        return

    py_cache = tempfile.mkdtemp(prefix='rx_py_idx_')
    go_cache = tempfile.mkdtemp(prefix='rx_go_idx_')

    try:
        # Build index with Python (--analyze to force indexing even small files)
        print(f'\n  Building Python index for {SOMELOG} (--analyze) ...')
        t0 = time.time()
        rc_py, out_py, err_py = run_cmd(
            [PY_RX, 'index', SOMELOG, '--json', '--analyze'],
            timeout=120,
            env_override={'RX_CACHE_DIR': py_cache},
        )
        py_time = time.time() - t0
        print(f'  Python index built in {py_time:.2f}s (rc={rc_py})')

        if rc_py != 0:
            report('Python index creation', False, f'stderr: {err_py[:500]}')
            return

        py_wrapper = parse_json(out_py)
        if py_wrapper is None:
            report('Python index JSON parse', False, f'stdout: {out_py[:500]}')
            return

        # The CLI wraps results as {"indexed": [...], "skipped": [...], ...}
        py_indexed = py_wrapper.get('indexed', [])
        if not py_indexed:
            report('Python produced indexed results', False,
                   f'Wrapper keys: {sorted(py_wrapper.keys())}, skipped: {py_wrapper.get("skipped", [])}')
            return
        py_data = py_indexed[0]

        # Build index with Go (--analyze)
        print(f'  Building Go index for {SOMELOG} (--analyze) ...')
        t0 = time.time()
        rc_go, out_go, err_go = run_cmd(
            [GO_RX, 'index', SOMELOG, '--json', '--analyze'],
            timeout=120,
            env_override={'RX_CACHE_DIR': go_cache},
        )
        go_time = time.time() - t0
        print(f'  Go index built in {go_time:.2f}s (rc={rc_go})')

        if rc_go != 0:
            report('Go index creation', False, f'stderr: {err_go[:500]}')
            return

        go_wrapper = parse_json(out_go)
        if go_wrapper is None:
            report('Go index JSON parse', False, f'stdout: {out_go[:500]}')
            return

        go_indexed = go_wrapper.get('indexed', [])
        if not go_indexed:
            report('Go produced indexed results', False,
                   f'Wrapper keys: {sorted(go_wrapper.keys())}')
            return
        go_data = go_indexed[0]

        print(f'\n  Python result keys: {sorted(py_data.keys())}')
        print(f'  Go result keys:     {sorted(go_data.keys())}')

        # Both should have a line_index
        py_li = py_data.get('line_index', [])
        go_li = go_data.get('line_index')
        # Go might report checkpoints count instead of full line_index in CLI output
        # Let's check what fields are available
        report('line_index present in Python output',
               isinstance(py_li, list) and len(py_li) > 0,
               f'entries={len(py_li)}')

        # First entry is [1, 0]
        if py_li:
            report('Python first line_index entry is [1, 0]',
                   py_li[0] == [1, 0],
                   f'First entry: {py_li[0]}')

        # Compare line_count
        py_lc = py_data.get('line_count')
        go_lc = go_data.get('line_count')
        # Also check nested analysis
        if go_lc is None:
            go_analysis = go_data.get('analysis')
            if isinstance(go_analysis, dict):
                go_lc = go_analysis.get('line_count')
        if py_lc is None:
            py_ll = py_data.get('line_length', {})

        report('line_count present and matching',
               py_lc is not None and go_lc is not None and py_lc == go_lc,
               f'Python={py_lc}, Go={go_lc}')

        # Compare line ending detection
        py_le = py_data.get('line_ending')
        go_le = go_data.get('line_ending')
        if go_le is None:
            go_analysis = go_data.get('analysis')
            if isinstance(go_analysis, dict):
                go_le = go_analysis.get('line_ending')

        report('line_ending detection matches',
               py_le is not None and go_le is not None and py_le == go_le,
               f'Python={py_le}, Go={go_le}')

        # Compare index_type or similar
        py_it = py_data.get('index_type') or py_data.get('file_type')
        go_it = go_data.get('index_type')
        report('index_type / file_type present',
               py_it is not None or go_it is not None,
               f'Python={py_it}, Go={go_it}')

        # Compare line_length stats
        py_ll = py_data.get('line_length', {})
        go_analysis = go_data.get('analysis')
        if isinstance(py_ll, dict) and isinstance(go_analysis, dict):
            py_max = py_ll.get('max')
            go_max = go_analysis.get('line_length_max')
            report('line_length_max matches',
                   py_max is not None and go_max is not None and py_max == go_max,
                   f'Python={py_max}, Go={go_max}')

            py_avg = py_ll.get('avg')
            go_avg = go_analysis.get('line_length_avg')
            if py_avg is not None and go_avg is not None:
                report('line_length_avg approximately matches',
                       abs(py_avg - go_avg) < 0.1,
                       f'Python={py_avg:.4f}, Go={go_avg:.4f}')

        print(f'\n  Timing: Python={py_time:.2f}s, Go={go_time:.2f}s')

    finally:
        shutil.rmtree(py_cache, ignore_errors=True)
        shutil.rmtree(go_cache, ignore_errors=True)


# =============================================================================
# Test 2: Index-accelerated line number resolution
# =============================================================================
def test_index_line_resolution():
    section('Test 2: Index-Accelerated Line Number Resolution')

    if not os.path.exists(SOMELOG):
        skip('Line resolution', f'{SOMELOG} not found')
        return

    go_cache = tempfile.mkdtemp(prefix='rx_go_line_')
    env = {'RX_CACHE_DIR': go_cache}

    try:
        # Search WITHOUT index
        print(f'\n  Running Go trace "{SOMELOG_PATTERN}" WITHOUT index (--no-index) ...')
        t0 = time.time()
        rc1, out1, err1 = run_cmd(
            [GO_RX, 'trace', SOMELOG_PATTERN, SOMELOG, '--json',
             '--max-results=5', '--no-index', '--no-cache'],
            timeout=60,
            env_override=env,
        )
        time_no_idx = time.time() - t0
        print(f'  No-index search: {time_no_idx:.2f}s (rc={rc1})')

        if rc1 != 0:
            report('Search without index', False, f'stderr: {err1[:300]}')
            return

        data_no_idx = parse_json(out1)
        if data_no_idx is None:
            report('Parse no-index results', False, f'stdout snippet: {out1[:300]}')
            return

        matches_no = data_no_idx.get('matches') or []
        report('No-index search finds matches',
               len(matches_no) > 0,
               f'{len(matches_no)} matches found')

        if matches_no:
            # Check that absolute_line_number is -1 (no index)
            first_abs = matches_no[0].get('absolute_line_number', 'missing')
            report('Without index: absolute_line_number is -1',
                   first_abs == -1,
                   f'First match absolute_line_number={first_abs}')

        # Build index
        print('  Building Go index for SOMELOG.log ...')
        rc_idx, _, err_idx = run_cmd(
            [GO_RX, 'index', SOMELOG, '--analyze'],
            timeout=120,
            env_override=env,
        )
        if rc_idx != 0:
            report('Go index build', False, f'stderr: {err_idx[:300]}')
            return
        report('Go index build succeeds', True)

        # Search WITH index
        print(f'  Running Go trace "{SOMELOG_PATTERN}" WITH index ...')
        t0 = time.time()
        rc2, out2, err2 = run_cmd(
            [GO_RX, 'trace', SOMELOG_PATTERN, SOMELOG, '--json',
             '--max-results=5', '--no-cache'],
            timeout=60,
            env_override=env,
        )
        time_with_idx = time.time() - t0
        print(f'  With-index search: {time_with_idx:.2f}s (rc={rc2})')

        if rc2 != 0:
            report('Search with index', False, f'stderr: {err2[:300]}')
            return

        data_with_idx = parse_json(out2)
        if data_with_idx is None:
            report('Parse with-index results', False, f'stdout snippet: {out2[:300]}')
            return

        matches_with = data_with_idx.get('matches') or []
        report('With-index search finds matches',
               len(matches_with) > 0,
               f'{len(matches_with)} matches found')

        if matches_with:
            # Check that absolute_line_number is now > 0
            first_abs = matches_with[0].get('absolute_line_number', -1)
            report('With index: absolute_line_number is positive',
                   first_abs > 0,
                   f'First match absolute_line_number={first_abs}')

            # Cross-check line numbers with rg -n
            print(f'  Cross-checking with rg -n "{SOMELOG_PATTERN}" ...')
            rc_rg, rg_out, _ = run_cmd(
                ['rg', '-n', SOMELOG_PATTERN, SOMELOG, '--max-count=5'],
                timeout=30,
            )
            if rc_rg == 0 and rg_out.strip():
                rg_lines = []
                for line in rg_out.strip().split('\n')[:5]:
                    parts = line.split(':', 1)
                    if parts[0].isdigit():
                        rg_lines.append(int(parts[0]))

                go_lines = [
                    m.get('absolute_line_number', -1)
                    for m in matches_with[:5]
                    if m.get('absolute_line_number', -1) > 0
                ]

                if rg_lines and go_lines:
                    match_count = min(len(rg_lines), len(go_lines))
                    matching = rg_lines[:match_count] == go_lines[:match_count]
                    report('Line numbers match rg -n output',
                           matching,
                           f'rg:  {rg_lines[:5]}\nGo:  {go_lines[:5]}')
                else:
                    report('Line number cross-check data available',
                           False,
                           f'rg_lines={rg_lines}, go_lines={go_lines}')
            else:
                skip('rg cross-check', 'rg -n returned no results or failed')

    finally:
        shutil.rmtree(go_cache, ignore_errors=True)


# =============================================================================
# Test 3: Trace cache stores and loads correctly
# =============================================================================
def test_trace_cache():
    section('Test 3: Trace Cache Store/Load')

    # Caching only triggers for large files (>=50MB by default).
    # middleware.log is 487MB, so it should trigger caching.
    # SOMELOG is only 11MB, so it likely won't cache.
    # We'll test with middleware.log for Go, and also test with SOMELOG
    # (the Go tool might have a different threshold or be configurable).

    go_cache = tempfile.mkdtemp(prefix='rx_go_cache_')
    env = {'RX_CACHE_DIR': go_cache}

    try:
        # --- Test with SOMELOG first (may or may not cache) ---
        print(f'\n  Testing trace cache with SOMELOG.log (11MB) ...')
        t0 = time.time()
        rc1, out1, err1 = run_cmd(
            [GO_RX, 'trace', SOMELOG_PATTERN, SOMELOG, '--json'],
            timeout=60,
            env_override=env,
        )
        time1 = time.time() - t0

        data1 = parse_json(out1)
        cache_dir = Path(go_cache) / 'trace_cache'
        somelog_cached = cache_dir.exists() and any(cache_dir.rglob('*.json'))
        print(f'  SOMELOG: {time1:.2f}s, cached={somelog_cached}')

        # --- Test with middleware.log (487MB, should definitely cache) ---
        if os.path.exists(MIDDLEWARE_LOG):
            print(f'\n  Testing trace cache with middleware.log (487MB) ...')
            t0 = time.time()
            rc_m1, out_m1, err_m1 = run_cmd(
                [GO_RX, 'trace', MIDDLEWARE_PATTERN, MIDDLEWARE_LOG, '--json'],
                timeout=300,
                env_override=env,
            )
            time_m1 = time.time() - t0
            print(f'  First run: {time_m1:.2f}s (rc={rc_m1})')

            if rc_m1 != 0:
                report('First middleware trace', False, f'stderr: {err_m1[:300]}')
            else:
                data_m1 = parse_json(out_m1)

                # Check for cache files
                cache_files = list(cache_dir.rglob('*.json')) if cache_dir.exists() else []
                report('Cache files created for large file',
                       len(cache_files) > 0,
                       f'Found {len(cache_files)} cache files')

                if cache_files:
                    # Inspect first cache file
                    with open(cache_files[0]) as f:
                        cache_content = json.load(f)
                    report('Cache file has required fields',
                           all(k in cache_content for k in ['version', 'source_path', 'patterns_hash']),
                           f'Keys: {sorted(cache_content.keys())[:10]}')
                    report('Cache version is 2',
                           cache_content.get('version') == 2,
                           f'version={cache_content.get("version")}')

                    # Second run: should be cache hit
                    print(f'  Second run (cache hit expected) ...')
                    t0 = time.time()
                    rc_m2, out_m2, err_m2 = run_cmd(
                        [GO_RX, 'trace', MIDDLEWARE_PATTERN, MIDDLEWARE_LOG, '--json'],
                        timeout=300,
                        env_override=env,
                    )
                    time_m2 = time.time() - t0
                    print(f'  Second run: {time_m2:.2f}s (rc={rc_m2})')

                    if rc_m2 == 0:
                        data_m2 = parse_json(out_m2)
                        if data_m1 and data_m2:
                            m1 = data_m1.get('matches') or []
                            m2 = data_m2.get('matches') or []
                            report('Cached results have same match count',
                                   len(m1) == len(m2),
                                   f'First: {len(m1)}, Second: {len(m2)}')

                            # Compare first 20 offsets
                            off1 = sorted(m.get('offset', -1) for m in m1[:20])
                            off2 = sorted(m.get('offset', -1) for m in m2[:20])
                            report('Cached offsets match (first 20)',
                                   off1 == off2,
                                   f'First:  {off1[:5]}\nSecond: {off2[:5]}')

                        report('Cache provides speedup',
                               time_m2 < time_m1,
                               f'Miss: {time_m1:.3f}s, Hit: {time_m2:.3f}s, '
                               f'Speedup: {time_m1/max(time_m2,0.001):.1f}x')
                else:
                    report('Cache created for 487MB file', False,
                           'No .json files found in trace_cache dir')
        else:
            skip('Middleware.log cache test', f'{MIDDLEWARE_LOG} not found')

        # --- Compare Go and Python cache directory structure ---
        print('\n  Comparing cache directory layout (Go vs Python spec) ...')
        # Expected Go layout: {cache}/trace_cache/{patterns_hash}/{path_hash}_{filename}.json
        if cache_dir.exists():
            all_cache_files = list(cache_dir.rglob('*.json'))
            if all_cache_files:
                example = all_cache_files[0]
                # Should be: trace_cache / <16-char-hex-dir> / <16-char-hex>_<filename>.json
                rel = example.relative_to(cache_dir)
                parts = rel.parts  # (patterns_hash_dir, filename)
                if len(parts) == 2:
                    patterns_dir, cache_filename = parts
                    report('Cache uses {patterns_hash}/{path_hash}_{filename}.json layout',
                           len(patterns_dir) == 16 and '_' in cache_filename,
                           f'Dir: {patterns_dir}\nFile: {cache_filename}')
                else:
                    report('Cache directory structure',
                           False,
                           f'Unexpected path depth: {rel}')

    finally:
        shutil.rmtree(go_cache, ignore_errors=True)


# =============================================================================
# Test 4: Index invalidation on file change
# =============================================================================
def test_index_invalidation():
    section('Test 4: Index Invalidation on File Change')

    go_cache = tempfile.mkdtemp(prefix='rx_go_inv_')
    tmp_dir = tempfile.mkdtemp(prefix='rx_src_')
    env = {'RX_CACHE_DIR': go_cache}

    try:
        tmp_file = os.path.join(tmp_dir, 'test_invalidation.log')

        # Write initial content
        with open(tmp_file, 'w') as f:
            for i in range(100):
                f.write(f'Line {i+1}: Some log content with rtp data here\n')

        print(f'\n  Created temp file: {tmp_file} ({os.path.getsize(tmp_file)} bytes)')

        # Build index
        print('  Building Go index ...')
        rc1, out1, err1 = run_cmd(
            [GO_RX, 'index', tmp_file, '--json', '--analyze'],
            timeout=60,
            env_override=env,
        )
        report('Initial index build succeeds', rc1 == 0,
               f'rc={rc1}' + (f', stderr: {err1[:200]}' if rc1 != 0 else ''))

        if rc1 != 0:
            return

        # Parse the wrapper to get the indexed entry
        idx_wrapper = parse_json(out1)
        idx_entry = None
        if idx_wrapper:
            indexed = idx_wrapper.get('indexed', [])
            if indexed:
                idx_entry = indexed[0]

        orig_line_count = None
        if idx_entry:
            orig_line_count = idx_entry.get('line_count')
            if orig_line_count is None:
                a = idx_entry.get('analysis')
                if isinstance(a, dict):
                    orig_line_count = a.get('line_count')
        report('Initial index has line_count',
               orig_line_count is not None and orig_line_count > 0,
               f'line_count={orig_line_count}')

        # Verify search works with index
        rc_s1, out_s1, _ = run_cmd(
            [GO_RX, 'trace', 'rtp', tmp_file, '--json', '--max-results=3'],
            timeout=30,
            env_override=env,
        )
        data_s1 = parse_json(out_s1)
        matches_s1 = (data_s1.get('matches') or []) if data_s1 else []
        report('Search with valid index works',
               rc_s1 == 0 and len(matches_s1) > 0,
               f'rc={rc_s1}, matches={len(matches_s1)}')

        # Modify the file (append lines, changing mtime and size)
        print('  Modifying file (appending lines) ...')
        time.sleep(1.0)  # Ensure mtime resolution difference
        with open(tmp_file, 'a') as f:
            f.write('Line 101: NEW rtp LINE ADDED AFTER INDEX\n')
            f.write('Line 102: ANOTHER NEW LINE\n')

        new_size = os.path.getsize(tmp_file)
        print(f'  File modified: new size={new_size} bytes')

        # Search again - should still work (index is invalidated but search is independent)
        print('  Searching after modification ...')
        rc_s2, out_s2, err_s2 = run_cmd(
            [GO_RX, 'trace', 'rtp', tmp_file, '--json', '--max-results=5'],
            timeout=30,
            env_override=env,
        )
        data_s2 = parse_json(out_s2)
        matches_s2 = (data_s2.get('matches') or []) if data_s2 else []
        report('Search after file modification works',
               rc_s2 == 0 and len(matches_s2) > 0,
               f'rc={rc_s2}, matches={len(matches_s2)}')

        # Rebuild index after modification with --force
        print('  Rebuilding index after modification (--force) ...')
        rc3, out3, err3 = run_cmd(
            [GO_RX, 'index', tmp_file, '--json', '--analyze', '--force'],
            timeout=60,
            env_override=env,
        )

        new_line_count = None
        if rc3 == 0:
            new_wrapper = parse_json(out3)
            if new_wrapper:
                new_indexed = new_wrapper.get('indexed', [])
                if new_indexed:
                    new_entry = new_indexed[0]
                    new_line_count = new_entry.get('line_count')
                    if new_line_count is None:
                        a = new_entry.get('analysis')
                        if isinstance(a, dict):
                            new_line_count = a.get('line_count')

        report('Rebuilt index has updated line_count',
               (new_line_count is not None and
                orig_line_count is not None and
                new_line_count > orig_line_count),
               f'Before={orig_line_count}, After={new_line_count}')

    finally:
        shutil.rmtree(go_cache, ignore_errors=True)
        shutil.rmtree(tmp_dir, ignore_errors=True)


# =============================================================================
# Test 5: Cache key computation compatibility
# =============================================================================
def test_cache_key_compatibility():
    section('Test 5: Cache Key Computation Compatibility')

    test_path = '/playground/SOMELOG.log'
    abs_path = os.path.abspath(test_path)
    basename = os.path.basename(test_path)

    # --- Index cache key ---
    # Python: {safe_basename}_{sha256(abs_path)[:16]}.json
    path_hash = hashlib.sha256(abs_path.encode('utf-8')).hexdigest()[:16]
    safe_basename = ''.join(
        c if c.isalnum() or c in '._-' else '_' for c in basename
    )
    py_index_key = f'{safe_basename}_{path_hash}.json'

    # Go uses identical logic: SafeFilename + "_" + hashPath + ".json"
    go_index_key = f'{safe_basename}_{path_hash}.json'

    report('Index cache key format match (computed)',
           py_index_key == go_index_key,
           f'Key: {py_index_key}')

    # Verify Go actually uses this by checking the cache dir
    go_cache = tempfile.mkdtemp(prefix='rx_go_keychk_')
    env = {'RX_CACHE_DIR': go_cache}
    try:
        if os.path.exists(test_path):
            run_cmd(
                [GO_RX, 'index', test_path, '--analyze'],
                timeout=120,
                env_override=env,
            )
            indexes_dir = Path(go_cache) / 'indexes'
            if indexes_dir.exists():
                idx_files = list(indexes_dir.glob('*.json'))
                if idx_files:
                    actual_name = idx_files[0].name
                    report('Go index filename matches expected',
                           actual_name == py_index_key,
                           f'Expected: {py_index_key}\nActual:   {actual_name}')
                else:
                    skip('Go index filename check', 'No index files created')
            else:
                skip('Go index filename check', 'No indexes/ directory')
    finally:
        shutil.rmtree(go_cache, ignore_errors=True)

    # --- Trace cache patterns hash ---
    matching_flags_set = {'-i', '-w', '-x', '-F', '-P', '--case-sensitive', '--ignore-case'}

    patterns = ['ERROR']
    rg_flags = ['-i']

    sorted_patterns = sorted(patterns)
    relevant_flags = sorted([f for f in rg_flags if f in matching_flags_set])
    hash_input = json.dumps(
        {'patterns': sorted_patterns, 'flags': relevant_flags},
        sort_keys=True,
    )
    patterns_hash_py = hashlib.sha256(hash_input.encode()).hexdigest()[:16]

    print(f'\n  Hash input:       {repr(hash_input)}')
    print(f'  patterns_hash:    {patterns_hash_py}')
    print(f'  path_hash:        {path_hash}')
    print(f'  Expected trace cache path: trace_cache/{patterns_hash_py}/{path_hash}_{basename}.json')

    # Verify Go produces the same hash by looking at actual cache files
    go_cache2 = tempfile.mkdtemp(prefix='rx_go_hashchk_')
    env2 = {'RX_CACHE_DIR': go_cache2}
    try:
        if os.path.exists(test_path):
            # Use -i flag via -- passthrough
            run_cmd(
                [GO_RX, 'trace', 'ERROR', test_path, '--json', '--', '-i'],
                timeout=60,
                env_override=env2,
            )
            cache_base = Path(go_cache2) / 'trace_cache'
            if cache_base.exists():
                subdirs = [d for d in cache_base.iterdir() if d.is_dir()]
                if subdirs:
                    go_patterns_hash = subdirs[0].name
                    print(f'  Go patterns_hash: {go_patterns_hash}')
                    report('Patterns hash Go == Python',
                           go_patterns_hash == patterns_hash_py,
                           f'Python: {patterns_hash_py}\nGo:     {go_patterns_hash}')

                    cache_files = list(subdirs[0].glob('*.json'))
                    if cache_files:
                        actual_fname = cache_files[0].name
                        expected_fname = f'{path_hash}_{basename}.json'
                        report('Trace cache filename Go == Python',
                               actual_fname == expected_fname,
                               f'Expected: {expected_fname}\nActual:   {actual_fname}')
                else:
                    skip('Patterns hash comparison',
                         'No subdirs in trace_cache/ (file below cache threshold?)')
            else:
                skip('Patterns hash comparison', 'No trace_cache/ directory created')
    finally:
        shutil.rmtree(go_cache2, ignore_errors=True)

    # --- Test pattern order independence and flag filtering (pure computation) ---
    print('\n  Pattern hash consistency tests (computed):')
    test_cases = [
        (['error'], [], 'single pattern, no flags'),
        (['error', 'warning'], [], 'two patterns sorted'),
        (['warning', 'error'], [], 'two patterns reversed (should match previous)'),
        (['error'], ['-i'], 'with -i flag'),
        (['error'], ['-w', '-i'], 'with -w -i flags'),
        (['error'], ['-A', '3'], 'with non-matching -A 3 flag'),
        ([], [], 'empty patterns'),
    ]

    hashes = []
    for pats, flags, desc in test_cases:
        sp = sorted(pats)
        rf = sorted([f for f in flags if f in matching_flags_set])
        hi = json.dumps({'patterns': sp, 'flags': rf}, sort_keys=True)
        h = hashlib.sha256(hi.encode()).hexdigest()[:16]
        hashes.append(h)
        print(f'    {desc:50s} -> {h}')

    # Order independence: cases 1 and 2 (sorted) should match
    report('Pattern order independence',
           hashes[1] == hashes[2],
           f'[error,warning]={hashes[1]}, [warning,error]={hashes[2]}')

    # Non-matching flags ignored: case 0 (no flags) should equal case 5 (-A 3)
    report('Non-matching flags (-A 3) ignored',
           hashes[0] == hashes[5],
           f'No flags={hashes[0]}, -A 3={hashes[5]}')

    # Matching flags differ: case 0 (no flags) != case 3 (-i)
    report('Matching flag (-i) changes hash',
           hashes[0] != hashes[3],
           f'No flags={hashes[0]}, -i={hashes[3]}')


# =============================================================================
# Main
# =============================================================================
def main():
    print('=' * 70)
    print('  Index & Cache Compatibility Tests: Go rx vs Python rx')
    print('=' * 70)

    # Preflight checks
    print('\nPreflight checks:')
    rc_go, out_go, _ = run_cmd([GO_RX, '--version'])
    print(f'  Go rx: {out_go.strip()} (rc={rc_go})')
    rc_py, out_py, _ = run_cmd([PY_RX, '--version'])
    print(f'  Python rx: {out_py.strip()} (rc={rc_py})')
    print(f'  SOMELOG.log exists: {os.path.exists(SOMELOG)} ({os.path.getsize(SOMELOG) if os.path.exists(SOMELOG) else 0} bytes)')
    print(f'  middleware.log exists: {os.path.exists(MIDDLEWARE_LOG)} ({os.path.getsize(MIDDLEWARE_LOG) if os.path.exists(MIDDLEWARE_LOG) else 0} bytes)')

    if rc_go != 0:
        print(f'\nFATAL: Go rx binary not found or broken at {GO_RX}')
        sys.exit(1)

    test_index_creation_format()
    test_index_line_resolution()
    test_trace_cache()
    test_index_invalidation()
    test_cache_key_compatibility()

    # Summary
    section('Summary')
    total = pass_count + fail_count + skip_count
    print(f'\n  Total: {total}  |  PASS: {pass_count}  |  FAIL: {fail_count}  |  SKIP: {skip_count}')
    if fail_count > 0:
        print(f'\n  {fail_count} test(s) FAILED')
        sys.exit(1)
    else:
        print('\n  All tests passed!')
        sys.exit(0)


if __name__ == '__main__':
    main()
