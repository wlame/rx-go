# systemd 257 on Debian stable: journalctl, systemd-journald and
# systemd-journal-remote, the reference for the journal fixtures.
# `just journal-fixtures` builds it as rx-journal-oracle:257 and runs
# make.sh in it.
#
# The base image is pinned by digest. The systemd packages come from
# Debian's archive at build time; the build fails unless their version
# is 257.x. zstd compresses the fixtures; python3 runs reference scripts
# that use this image.
FROM debian:stable-slim@sha256:eb593cf2c358cacef45ca0a424bbc7d30cfa3466265fc2662b9466a0ca6ba1c5

RUN apt-get update -qq \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
        systemd systemd-journal-remote zstd python3 >/dev/null \
 && rm -rf /var/lib/apt/lists/* \
 && version=$(dpkg-query --show --showformat='${Version}' systemd) \
 && case "$version" in 257.*) ;; *) echo "systemd $version is not 257" >&2; exit 1 ;; esac
