#!/usr/bin/env bash
# Writes the journal fixtures of internal/journal into /testdata.
#
# Runs inside the image built from the Dockerfile beside it, with the
# generator of this directory, built for Linux, on the PATH as
# journal-fixture-gen and internal/journal/testdata mounted at /testdata.
# `just journal-fixtures` sets that up and passes its arguments here:
#
#   make.sh                 write every fixture
#   make.sh --skip-chains   keep the chain fixtures as they are
#
# Set fixtures: for each set the generator writes and each variant
# (SYSTEMD_JOURNAL_COMPRESS NONE, XZ, LZ4, ZSTD x SYSTEMD_JOURNAL_COMPACT
# 0, 1), systemd-journal-remote turns the set's export stream into a
# journal. The journal is trimmed to its used size, checked against the
# stream, and stored as SET-COMPRESSION-MODE.journal.zst. The text
# journalctl prints must be the same bytes for the eight variants of a
# set (compression and compact mode do not change it); it is stored once,
# as SET.txt.
#
# Chain fixtures: systemd-journald itself writes invented messages sent
# with systemd-cat, rotated between batches, into chain-one-counter/ and
# chain-two-counters/ (whose second journald starts a new counter), with
# all.txt, the text journalctl prints over every part. Their times come
# from the clock, so each run writes them anew.
#
# Last, fixtures.json is written from every stored journal.
set -euo pipefail

gen=journal-fixture-gen
remote=/usr/lib/systemd/systemd-journal-remote
journald=/usr/lib/systemd/systemd-journald
out=/testdata
work=$(mktemp -d)
stage=$work/stage

# An invented machine id: journald names its directory after it.
machine_id=0123456789abcdef0123456789abcdef
runtime_dir=/run/log/journal/$machine_id

skip_chains=false
for arg in "$@"; do
    case "$arg" in
        --skip-chains) skip_chains=true ;;
        *) echo "make.sh: unknown argument: $arg" >&2; exit 2 ;;
    esac
done

# jctl runs journalctl with only the environment the fixtures' text is
# defined for: UTC, the C locale, no SYSTEMD_COLORS, no SYSTEMD_URLIFY.
jctl() {
    env -i PATH="$PATH" TZ=UTC LC_ALL=C journalctl --no-pager "$@"
}

# text prints the text of the journals named by --file= arguments.
text() {
    jctl --utc -q -o short-iso-precise "$@"
}

# store compresses a journal into the stage.
store() {
    zstd -19 -q "$1" -o "$2"
    chmod 644 "$2"
}

make_set_fixtures() {
    local set compression mode compact name raw journal first
    mkdir -p "$work/streams" "$work/raw" "$work/journals" "$work/export" "$work/text" "$stage"
    "$gen" streams --out="$work/streams"
    for set in basic many stamps; do
        first=
        for compression in none xz lz4 zstd; do
            for mode in regular compact; do
                name=$set-$compression-$mode
                raw=$work/raw/$name.journal
                journal=$work/journals/$name.journal
                compact=0
                if [ "$mode" = compact ]; then compact=1; fi
                env -i PATH="$PATH" SYSTEMD_JOURNAL_COMPRESS="${compression^^}" SYSTEMD_JOURNAL_COMPACT="$compact" \
                    "$remote" --output="$raw" "$work/streams/$set.export"
                "$gen" trim --in="$raw" --out="$journal"
                jctl --file="$journal" -o export > "$work/export/$name.export"
                "$gen" check --sent="$work/streams/$set.export" --stored="$work/export/$name.export" \
                    --journal="$journal" --compression="$compression" --compact="$compact"
                text --file="$journal" > "$work/text/$name.txt"
                if [ -z "$first" ]; then
                    first=$work/text/$name.txt
                elif ! cmp -s "$first" "$work/text/$name.txt"; then
                    echo "make.sh: the text of $name differs from $(basename "$first")" >&2
                    exit 1
                fi
                store "$journal" "$stage/$name.journal.zst"
                echo "wrote $name"
            done
        done
        cp "$first" "$stage/$set.txt"
        chmod 644 "$stage/$set.txt"
        echo "wrote $set.txt"
    done
}

# configure_journald sets up a journald that runs alone: it keeps its
# journals in /run, reads no kernel log and forwards nothing. The sizes
# its own messages report are those of the container's /run, a tmpfs of
# a fixed size, and of the limits set here.
configure_journald() {
    echo "$machine_id" > /etc/machine-id
    mkdir -p /etc/systemd
    cat > /etc/systemd/journald.conf <<'CONF'
[Journal]
Storage=volatile
SyncIntervalSec=1h
RateLimitBurst=0
ReadKMsg=no
ForwardToSyslog=no
ForwardToWall=no
ForwardToConsole=no
RuntimeMaxUse=64M
RuntimeKeepFree=16M
RuntimeMaxFileSize=8M
CONF
}

# start_journald starts journald in the background and waits for the
# socket systemd-cat writes to.
start_journald() {
    "$journald" >> "$work/journald.log" 2>&1 &
    journald_pid=$!
    for _ in $(seq 100); do
        if [ -S /run/systemd/journal/stdout ]; then return 0; fi
        sleep 0.1
    done
    echo "make.sh: systemd-journald did not start" >&2
    cat "$work/journald.log" >&2
    exit 1
}

# stop_journald stops journald, which closes its journals.
stop_journald() {
    kill -TERM "$journald_pid"
    wait "$journald_pid" || true
    rm -f /run/systemd/journal/stdout
}

# forget_journald removes every journal and the counter journald keeps.
forget_journald() {
    rm -rf /run/log/journal /run/systemd/journal
    mkdir -p /run/systemd/journal
}

# write_batch sends three invented lines through systemd-cat and waits
# until journald has written them.
write_batch() {
    local chain=$1 batch=$2 line
    for line in 1 2 3; do
        printf '%s batch %d line %d\n' "$chain" "$batch" "$line" \
            | systemd-cat --identifier=chaintest --priority=info
    done
    journalctl --sync
    sleep 1
}

# rotate makes journald archive the active journal and start a new one.
rotate() {
    journalctl --rotate
}

# store_chain trims every journal journald wrote into the chain's
# directory in the stage, checks how many parts and counters they have,
# and writes all.txt over them. The active journal is read while journald
# runs, as a copy taken from a live system would be.
store_chain() {
    local chain=$1 want_counters=$2 part trimmed counters
    local trimmed_dir=$work/chains/$chain files=()
    mkdir -p "$trimmed_dir" "$stage/$chain"
    for part in "$runtime_dir"/*.journal; do
        trimmed=$trimmed_dir/$(basename "$part")
        "$gen" trim --in="$part" --out="$trimmed"
        files+=("--file=$trimmed")
    done
    if [ "${#files[@]}" -ne 3 ]; then
        echo "make.sh: $chain has ${#files[@]} parts, want 3" >&2
        exit 1
    fi
    counters=$(for part in "$trimmed_dir"/*.journal; do
        jctl --header --file="$part" | sed -n 's/^Sequential number ID: //p'
    done | sort -u | wc -l)
    if [ "$counters" -ne "$want_counters" ]; then
        echo "make.sh: $chain has $counters counters, want $want_counters" >&2
        exit 1
    fi
    text "${files[@]}" > "$stage/$chain/all.txt"
    for part in "$trimmed_dir"/*.journal; do
        store "$part" "$stage/$chain/$(basename "$part").zst"
    done
    echo "wrote $chain"
}

# make_chain_one_counter: three batches, rotated between them, so one
# counter numbers the active journal and its two archives.
make_chain_one_counter() {
    forget_journald
    start_journald
    write_batch one-counter 1
    rotate
    write_batch one-counter 2
    rotate
    write_batch one-counter 3
    store_chain chain-one-counter 1
    stop_journald
}

# make_chain_two_counters: two batches and two rotations under one
# journald; then a second journald writes the third batch with a new
# counter. journald keeps its counter in /run/systemd/journal/seqnum and
# otherwise takes the active journal's, so both are removed before it
# starts. The journal removed holds only journald's own message written
# after the second rotation.
make_chain_two_counters() {
    forget_journald
    start_journald
    write_batch two-counters 1
    rotate
    write_batch two-counters 2
    rotate
    stop_journald
    rm -f /run/systemd/journal/seqnum "$runtime_dir/system.journal"
    start_journald
    write_batch two-counters 3
    store_chain chain-two-counters 2
    stop_journald
}

# install_fixtures copies the stage into /testdata. A chain's directory
# is replaced whole, since its archives' names change with every run.
install_fixtures() {
    local chain
    cp "$stage"/*.txt "$stage"/*.journal.zst "$out"/
    if [ "$skip_chains" = true ]; then return 0; fi
    for chain in chain-one-counter chain-two-counters; do
        rm -rf "${out:?}/$chain"
        cp -R "$stage/$chain" "$out/$chain"
    done
}

# write_manifest writes fixtures.json from every journal in /testdata:
# each is decompressed beside its `journalctl -o export` output and its
# text, for the generator to count.
write_manifest() {
    local manifest_dir=$work/manifest zst rel name
    mkdir -p "$manifest_dir"
    while IFS= read -r zst; do
        rel=${zst#"$out"/}
        name=${rel%.journal.zst}
        mkdir -p "$manifest_dir/$(dirname "$name")"
        zstd -d -q "$zst" -o "$manifest_dir/$name.journal"
        jctl --file="$manifest_dir/$name.journal" -o export > "$manifest_dir/$name.export"
        text --file="$manifest_dir/$name.journal" > "$manifest_dir/$name.txt"
    done < <(find "$out" -maxdepth 2 -name '*.journal.zst' | sort)
    "$gen" manifest --work="$manifest_dir" > "$work/fixtures.json"
    cp "$work/fixtures.json" "$out/fixtures.json"
    echo "wrote fixtures.json"
}

make_set_fixtures
if [ "$skip_chains" = false ]; then
    configure_journald
    make_chain_one_counter
    make_chain_two_counters
fi
install_fixtures
write_manifest
