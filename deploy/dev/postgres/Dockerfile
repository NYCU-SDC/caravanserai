# cara-server's control-plane Postgres, with WAL-G available inside the
# container.
#
# WAL-G has to live here rather than on the host because Postgres invokes
# archive_command itself: the command runs in this container's namespace, with
# this container's filesystem and environment. A wal-g on the host would never
# be reached.
#
# The image carries no credentials. WALG_* and AWS_* are supplied at runtime by
# compose (development) or by the deployment (production, see CARA-77); baking
# them into a layer would leave them recoverable from the image forever.

# Pinned to a patch version, not the moving `16` tag. Restore correctness does
# not require it — PGDATA compatibility is a major-version property — but a
# backup system's value rests on the restore path behaving exactly as it did
# when it was tested, and a rebuilt image that quietly differs undermines that.
FROM postgres:16.13

# Pinned deliberately. `latest` would make the image's contents depend on when
# it happened to be built, which is the opposite of what a backup tool needs.
ARG WALG_VERSION=v3.0.8

# Ubuntu 22.04 builds (glibc 2.35) are chosen to match this image's Debian
# bookworm base (glibc 2.36): a binary linked against an older glibc runs on a
# newer one, but not the reverse. The 24.04 assets (glibc 2.39) would fail at
# exec time rather than at build time, which is the worst place to find out.
ARG WALG_UBUNTU=22.04

# Published by the WAL-G release, not computed by us. Update both when bumping
# WALG_VERSION; a mismatch fails the build rather than silently installing
# whatever was served.
ARG WALG_SHA256_AMD64=b0df1b484035eb5f131db7bbd303d1a460391848fdcce34ba1e0a564cca493e9
ARG WALG_SHA256_ARM64=048b5033c34c1487dc1fccc0572db030e323230984ccc093e80e4e4081c797d1

# Supplied automatically by BuildKit. Each machine builds for itself, so the
# dev compose needs no cross-architecture setup; publishing a single
# multi-architecture image is CARA-77's problem.
ARG TARGETARCH

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) walg_arch='amd64';   walg_sha="${WALG_SHA256_AMD64}" ;; \
      arm64) walg_arch='aarch64'; walg_sha="${WALG_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    rm -rf /var/lib/apt/lists/*; \
    \
    asset="wal-g-pg-${WALG_UBUNTU}-${walg_arch}.tar.gz"; \
    curl -fsSL -o /tmp/walg.tar.gz \
      "https://github.com/wal-g/wal-g/releases/download/${WALG_VERSION}/${asset}"; \
    echo "${walg_sha}  /tmp/walg.tar.gz" | sha256sum -c -; \
    \
    tar -xzf /tmp/walg.tar.gz -C /tmp; \
    mv "/tmp/wal-g-pg-${WALG_UBUNTU}-${walg_arch}" /usr/local/bin/wal-g; \
    chmod 0755 /usr/local/bin/wal-g; \
    rm -f /tmp/walg.tar.gz; \
    \
    # Proves the binary matches the architecture it was selected for. Without
    # this the mismatch would surface much later, as a failing archive_command
    # on a running cluster.
    wal-g --version
