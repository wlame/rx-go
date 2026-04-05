# Extended Benchmark Guide

## Overview

**Comprehensive benchmark with 78+ test scenarios** covering:

✅ **Full file scans** (no --max-results) - Compare ALL matches
✅ **Extensive context testing** - 9 different before/after combinations
✅ **rx samples subcommand** - Sample extraction validation
✅ **8 diverse patterns** - Including regex and non-ASCII
✅ **Multiple suites** - Edge cases, formats, multi-pattern, etc.

## Quick Start

### Transfer & Setup

```bash
# Transfer to MacBook
scp rx-benchmark-extended-*.tar.gz mac.local:~/

# Extract
tar -xzf rx-benchmark-extended-*.tar.gz
cd rx-benchmark-extended

# Link Python binary
ln -s /path/to/rx-python/bin/rx rx-py

# Verify
./dist/rx-darwin-arm64 --version
./rx-py --version
```

### Run Benchmark

```bash
# Full suite (~1-2 hours)
./test/comparison/wiki-benchmark-extended.sh /path/to/wiki.xml

# Or run on smaller file first to test
./test/comparison/wiki-benchmark-extended.sh /path/to/smaller-test.xml
```

## Test Patterns

### 8 Diverse Patterns

1. **rare:** "Italian Protestant communities" (very rare, ~1 match)
2. **medium:** "Pennsylvania National Guard" (~594 matches)
3. **common:** "yet to release" (~419 matches)
4. **river:** "Delaware River" (new)
5. **war:** "Russo-Ukrainian War" (new)
6. **russian:** "нетрадиционны. отношени.{1,3}" (non-ASCII + regex)
7. **movie:** "Simpsons Movie" (new)
8. **music:** "six-mallet" (very rare)

## Test Suites (78+ tests)

### Suite 1: Full File Scans (16 tests)
**No --max-results - finds ALL matches**

- Tests all 8 patterns
- Both Go and Python
- **Purpose:** Compare total match counts

Expected outcome:
- ✅ Go matches = Python matches for each pattern
- 📊 Complete match count data

### Suite 2: Limited Scans (16 tests)
**Streaming validation**

- Limits: 1, 5, 10, 50
- Patterns: common, river
- Both implementations

Expected outcome:
- ✅ Exact limit enforcement
- ⚡ Early termination (time scales with limit)

### Suite 3: Context Variations (18 tests)
**Extensive context testing**

Context combinations:
- 0:0 (no context)
- 1:0, 0:1, 1:1 (minimal)
- 3:0, 0:3, 3:3 (standard)
- 5:5, 10:10 (large)

Expected outcome:
- ✅ Identical context lines Go vs Python
- 📊 Context overhead measurements

### Suite 4: Multi-Pattern (4 tests)

- All 8 patterns combined (no limit)
- 3-pattern subset (limit=100)
- Both implementations

Expected outcome:
- ⚡ Single-pass faster than multiple searches
- ✅ Correct match distribution

### Suite 5: Output Formats (4 tests)

- JSON vs plain text
- Same pattern (river)
- Both implementations

Expected outcome:
- ✅ Identical match counts
- 📊 Format overhead comparison

### Suite 6: Samples Subcommand (10 tests)

**Tests rx samples** - NEW!

- Extract by byte offset
- Various context sizes (0, 1, 5, 10)
- Both implementations

Expected outcome:
- ✅ Identical sample content
- ✅ Correct context extraction

### Suite 7: Regex Patterns (6 tests)

- Literal patterns
- Regex with quantifiers
- Case-insensitive
- Both implementations

Expected outcome:
- ✅ Regex correctly handled
- ✅ Non-ASCII patterns work

### Suite 8: Edge Cases (4 tests)

- Very rare patterns
- Multi-word patterns
- Both implementations

Expected outcome:
- ✅ Handles edge cases correctly
- ✅ No crashes or errors

## Expected Runtime

On 12-core ARM MacBook with 75GB Wikipedia file:

| Suite | Tests | Est. Time |
|-------|-------|-----------|
| Full Scans | 16 | 30-60 min |
| Limited | 16 | 5-15 min |
| Context | 18 | 5-10 min |
| Multi-Pattern | 4 | 5-10 min |
| Formats | 4 | 2-5 min |
| Samples | 10 | 2-5 min |
| Regex | 6 | 2-5 min |
| Edge Cases | 4 | 2-5 min |
| **Total** | **78+** | **~1-2 hours** |

## Output Structure

```
results/extended-YYYYMMDD-HHMMSS/
├── extended-report.md              # Summary report
├── extended-data.csv               # All test data
└── logs/
    ├── full-rare-go.log            # Test outputs
    ├── full-rare-go.json           # Metadata
    ├── full-rare-go.time           # Timing
    ├── full-rare-py.log
    ├── full-rare-py.json
    ├── full-rare-py.time
    └── ... (~156 files)
```

## Key Validations

### ✅ Match Count Consistency

```bash
# After benchmark completes
cd results/extended-*/

# Compare match counts by pattern
awk -F, 'NR>1 && $1 ~ /^full-/ {print $1, $2, $4}' extended-data.csv | sort
```

Should see:
```
full-common go 419    full-common py 419    ✓ MATCH
full-medium go 594    full-medium py 594    ✓ MATCH
full-rare go 1        full-rare py 1        ✓ MATCH
...
```

### ⚡ Streaming Validation

```bash
# Limited scan times should scale with limit
grep "limit1-" extended-data.csv
grep "limit50-" extended-data.csv
```

Expected: limit1 << limit50 (not proportional to file size)

### 📊 Context Overhead

```bash
# Context extraction overhead
awk -F, '$1 ~ /^ctx-/ {print $1, $3}' extended-data.csv | sort -t- -k2,2n
```

Expected: ctx-0-0 < ctx-1-1 < ctx-3-3 < ctx-10-10

### 🔍 Samples Validation

```bash
# Samples should return identical content
diff logs/samples-bytes-go.log logs/samples-bytes-py.log
```

Expected: No differences (or minimal formatting only)

## Monitoring Progress

The script provides real-time progress:

```
[SUITE] Suite 1: Full File Scans (No Limits)
============================================================
[INFO] Pattern: rare = "Italian Protestant communities"
[TEST] full-rare (go)
[✓] Done: 234.52s, 1 matches
[TEST] full-rare (py)
[✓] Done: 241.15s, 1 matches

[INFO] Pattern: medium = "Pennsylvania National Guard"
[TEST] full-medium (go)
[✓] Done: 189.34s, 594 matches
...
```

## Results Analysis

### Bring Back

```bash
cd results
tar -czf extended-results-$(date +%Y%m%d).tar.gz extended-*/
# Transfer back to Linux
```

### Quick Analysis Commands

```bash
# Total tests
wc -l < extended-data.csv

# Match count summary
awk -F, 'NR>1 {impl[$2]++; matches[$2]+=$4} END {for (i in impl) print i, impl[i], "tests,", matches[i], "total matches"}' extended-data.csv

# Performance summary (Go vs Py)
awk -F, 'NR>1 {time[$2]+=$3; tests[$2]++} END {for (i in time) printf "%s: %.2f avg sec/test\n", i, time[i]/tests[i]}' extended-data.csv

# Find mismatches
awk -F, 'NR>1 {key=$1; impl=$2; count=$4; if (key in data && data[key]!=count) print "MISMATCH:", key, data_impl[key]"="data[key], impl"="count; data[key]=count; data_impl[key]=impl}' extended-data.csv
```

## Differences from Basic Benchmark

| Feature | Basic | Extended |
|---------|-------|----------|
| Tests | 26 | 78+ |
| Full scans | No | Yes (8 patterns) |
| Context tests | 6 | 18 |
| Samples tests | 0 | 10 |
| Patterns | 3 | 8 |
| Regex patterns | 0 | 3 |
| Non-ASCII | No | Yes (Russian) |
| Runtime | ~30-60 min | ~1-2 hours |

## Troubleshooting

Same as basic benchmark, plus:

### Samples not running
- Ensure first match finding completes
- Check `logs/sample-matches.json` exists
- Verify jq is installed

### Non-ASCII pattern issues
- Ensure terminal supports UTF-8
- Check Wikipedia file encoding
- Verify ripgrep locale settings

### Memory issues
- Extended benchmark uses more memory for full scans
- Monitor with: `top -pid $(pgrep rx)`
- Reduce concurrent tests if needed

## Next Steps After Benchmark

1. **Transfer results back**
2. **Review extended-report.md**
3. **Analyze CSV data** for patterns
4. **Investigate anomalies** if any
5. **Compare with basic benchmark** results

---

**Ready for comprehensive testing!** 🚀

This extended benchmark will give you complete validation of:
- Match count accuracy across all patterns
- Performance characteristics at scale
- Context extraction correctness
- Sample extraction functionality
- Regex and non-ASCII support
