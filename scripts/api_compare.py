#!/usr/bin/env python3
"""Compare Python and Go rx API responses for frontend compatibility."""
import json
import sys
import urllib.request
import urllib.parse

PY = 'http://localhost:4799'
GO = 'http://localhost:4800'

def fetch(url):
    """Fetch JSON from URL."""
    try:
        with urllib.request.urlopen(url, timeout=120) as r:
            return json.loads(r.read())
    except Exception as e:
        return {'_error': str(e)}

def compare_keys(py_data, go_data, label):
    """Compare JSON structure: keys presence, types, null handling."""
    if isinstance(py_data, dict) and isinstance(go_data, dict):
        py_keys = set(py_data.keys())
        go_keys = set(go_data.keys())

        missing = py_keys - go_keys
        extra = go_keys - py_keys

        issues = []
        if missing:
            issues.append(f'MISSING from Go: {missing}')
        if extra:
            issues.append(f'EXTRA in Go: {extra}')

        # Check type compatibility for shared keys
        for k in py_keys & go_keys:
            pv = py_data[k]
            gv = go_data[k]
            # null vs non-null
            if pv is None and gv is not None:
                issues.append(f'{k}: Python=null Go={type(gv).__name__}({repr(gv)[:50]})')
            elif pv is not None and gv is None:
                issues.append(f'{k}: Python={type(pv).__name__}({repr(pv)[:50]}) Go=null')
            elif type(pv) != type(gv):
                issues.append(f'{k}: type mismatch Python={type(pv).__name__} Go={type(gv).__name__}')

        return issues
    return []

def test_endpoint(path, params=None, label=None):
    """Compare Python and Go responses for an endpoint."""
    if label is None:
        label = path

    query = ''
    if params:
        parts = []
        for k, v in params.items():
            if isinstance(v, list):
                for item in v:
                    parts.append(f'{k}={urllib.parse.quote(str(item))}')
            else:
                parts.append(f'{k}={urllib.parse.quote(str(v))}')
        query = '?' + '&'.join(parts)

    py_url = f'{PY}{path}{query}'
    go_url = f'{GO}{path}{query}'

    sys.stdout.write(f'\n{"="*60}\n{label}\n  {path}{query}\n')
    sys.stdout.flush()

    py_data = fetch(py_url)
    go_data = fetch(go_url)

    if '_error' in py_data:
        print(f'  Python ERROR: {py_data["_error"]}')
        return False
    if '_error' in go_data:
        print(f'  Go ERROR: {go_data["_error"]}')
        return False

    # Compare structure
    issues = compare_keys(py_data, go_data, label)

    # Show key fields
    py_keys = sorted(py_data.keys())
    go_keys = sorted(go_data.keys())
    print(f'  Python keys ({len(py_keys)}): {py_keys}')
    print(f'  Go keys ({len(go_keys)}):     {go_keys}')

    if issues:
        print(f'  ISSUES ({len(issues)}):')
        for issue in issues:
            print(f'    - {issue}')
        return False
    else:
        print(f'  Structure: OK (all {len(py_keys)} keys match)')
        return True

def test_trace(params, label):
    """Compare /v1/trace responses in detail."""
    query_parts = []
    for k, v in params.items():
        if isinstance(v, list):
            for item in v:
                query_parts.append(f'{k}={urllib.parse.quote(str(item))}')
        else:
            query_parts.append(f'{k}={urllib.parse.quote(str(v))}')
    query = '&'.join(query_parts)

    sys.stdout.write(f'\n{"="*60}\n{label}\n  /v1/trace?{query}\n')
    sys.stdout.flush()

    py_data = fetch(f'{PY}/v1/trace?{query}')
    go_data = fetch(f'{GO}/v1/trace?{query}')

    if '_error' in py_data:
        print(f'  Python ERROR: {py_data["_error"]}')
        return False
    if '_error' in go_data:
        print(f'  Go ERROR: {go_data["_error"]}')
        return False

    # Top-level keys
    issues = compare_keys(py_data, go_data, label)

    py_keys = sorted(py_data.keys())
    go_keys = sorted(go_data.keys())
    print(f'  Python keys ({len(py_keys)}): {py_keys}')
    print(f'  Go keys ({len(go_keys)}):     {go_keys}')

    # Volatile fields (expected to differ)
    volatile = {'time', 'request_id', 'cli_command'}

    # Match counts
    py_matches = py_data.get('matches') or []
    go_matches = go_data.get('matches') or []
    print(f'  Matches: Python={len(py_matches)} Go={len(go_matches)}')

    # Compare match structure (first match)
    if py_matches and go_matches:
        pm = py_matches[0]
        gm = go_matches[0]
        match_issues = compare_keys(pm, gm, 'match[0]')
        print(f'  Match keys:')
        print(f'    Python: {sorted(pm.keys())}')
        print(f'    Go:     {sorted(gm.keys())}')
        if match_issues:
            for mi in match_issues:
                print(f'    - {mi}')
            issues.extend(match_issues)

        # Compare first few matches by offset
        match_ok = 0
        match_diff = 0
        for i in range(min(len(py_matches), len(go_matches), 10)):
            if py_matches[i].get('offset') == go_matches[i].get('offset'):
                match_ok += 1
            else:
                match_diff += 1
        print(f'  First 10 match offsets: {match_ok} OK, {match_diff} DIFF')

    # Non-volatile field comparison
    for k in sorted(set(py_data.keys()) & set(go_data.keys()) - volatile):
        pv = py_data[k]
        gv = go_data[k]
        if k == 'matches':
            continue  # handled above
        if pv != gv and not (pv is None and gv is None):
            # For lists/dicts, just compare lengths
            if isinstance(pv, (list, dict)) and isinstance(gv, (list, dict)):
                if len(pv) != len(gv):
                    issues.append(f'{k}: length py={len(pv)} go={len(gv)}')
            else:
                issues.append(f'{k}: py={repr(pv)[:80]} go={repr(gv)[:80]}')

    if issues:
        print(f'  ISSUES ({len(issues)}):')
        for issue in [i for i in issues if not any(v in i for v in volatile)]:
            print(f'    - {issue}')
        return False
    else:
        print(f'  ALL OK')
        return True

def main():
    results = []

    # 1. Health
    results.append(('GET /health', test_endpoint('/health')))

    # 2. Detectors
    results.append(('GET /v1/detectors', test_endpoint('/v1/detectors')))

    # 3. Complexity (stub)
    results.append(('GET /v1/complexity', test_endpoint('/v1/complexity', {'regex': '(a+)+'})))

    # 4. Trace — small file
    results.append(('TRACE small.txt', test_trace(
        {'path': '/playground/small.txt', 'regexp': 'hotmail'},
        'Trace: small.txt / hotmail'
    )))

    # 5. Trace — medium file
    results.append(('TRACE SOMELOG/AudioDevice', test_trace(
        {'path': '/playground/SOMELOG.log', 'regexp': 'AudioDevice'},
        'Trace: SOMELOG.log / AudioDevice'
    )))

    # 6. Trace — with max_results
    results.append(('TRACE middleware max10', test_trace(
        {'path': '/playground/middleware.log-2025121008', 'regexp': 'ERROR', 'max_results': 10},
        'Trace: middleware.log / ERROR (max 10)'
    )))

    # 7. Samples
    results.append(('GET /v1/samples', test_endpoint(
        '/v1/samples',
        {'path': '/playground/SOMELOG.log', 'byte_offset': [0, 1000]},
        'Samples: SOMELOG.log offsets 0,1000'
    )))

    # 8. Metrics
    sys.stdout.write(f'\n{"="*60}\nGET /metrics\n')
    sys.stdout.flush()
    try:
        py_resp = urllib.request.urlopen(f'{PY}/metrics', timeout=5).read().decode()
        go_resp = urllib.request.urlopen(f'{GO}/metrics', timeout=5).read().decode()
        py_has_prom = '# HELP' in py_resp or '# TYPE' in py_resp
        go_has_prom = '# HELP' in go_resp or '# TYPE' in go_resp
        print(f'  Python: {"prometheus format" if py_has_prom else "NOT prometheus"} ({len(py_resp)} bytes)')
        print(f'  Go:     {"prometheus format" if go_has_prom else "NOT prometheus"} ({len(go_resp)} bytes)')
        results.append(('GET /metrics', py_has_prom and go_has_prom))
    except Exception as e:
        print(f'  ERROR: {e}')
        results.append(('GET /metrics', False))

    # Summary
    print(f'\n{"="*60}')
    print('API COMPATIBILITY SUMMARY')
    print(f'{"="*60}')
    total_pass = 0
    total_fail = 0
    for name, ok in results:
        status = 'PASS' if ok else 'FAIL'
        if ok:
            total_pass += 1
        else:
            total_fail += 1
        print(f'  {status}: {name}')
    print(f'\n  Total: {total_pass} PASS, {total_fail} FAIL out of {len(results)}')

if __name__ == '__main__':
    main()
