# ghcr.io/nycu-sdc/cara-postgres:16 — the PostgreSQL image a user must run to
# get PostgresDifferential backups, and the image the Node's pgBackRest repo
# host runs from. The contract this image offers (paths, user, run modes) is in
# the README next to this file; Cara's validation, placement and backup code
# depend on it.
#
# The image carries no repository, stanza, endpoint, certificate or credential.
# The Agent mounts all of them at runtime; anything baked into a layer would be
# recoverable from the image forever.

# The major version and the Debian release are pinned; the patch version is not.
# PGDATA compatibility is a major-version property, and PostgreSQL minor
# releases are mostly security and bug fixes, so letting them drift forward is
# the safe direction (same reasoning as deploy/dev/postgres/Dockerfile).
#
# The Debian release is pinned because the pgBackRest package below is built
# per release: its version string carries the release (pgdg13 is trixie), and
# a base that moved to the next release would fail to install it.
FROM postgres:16-trixie

# Exact, because pgBackRest's TLS client and server must run the same version:
# the repo host runs from this same image, and a version that depended on when
# the image happened to be built would let two Nodes drift apart. The PGDG
# repository keeps earlier versions, so a pinned build stays reproducible after
# a newer release appears.
ARG PGBACKREST_VERSION=2.59.3-1.pgdg13+1

# PG_VERSION is set by the base image, so the label reports the minor actually
# installed rather than one written here and left to go stale.
LABEL org.opencontainers.image.title="cara-postgres" \
      org.opencontainers.image.source="https://github.com/NYCU-SDC/caravanserai" \
      dev.cara.postgres.version="${PG_VERSION}" \
      dev.cara.pgbackrest.version="${PGBACKREST_VERSION}" \
      dev.cara.base.image="postgres:16-trixie"

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      "pgbackrest=${PGBACKREST_VERSION}" \
      tini; \
    rm -rf /var/lib/apt/lists/*; \
    \
    # The package ships a sample config, and pgBackRest falls back to
    # /etc/pgbackrest.conf whenever /etc/pgbackrest/pgbackrest.conf is absent.
    # Removing both means a container without the Agent's mounted config runs
    # on pgBackRest defaults, not on whatever the package author chose.
    rm -f /etc/pgbackrest.conf /etc/pgbackrest/pgbackrest.conf; \
    mkdir -p /etc/pgbackrest /etc/cara/pgbackrest/tls; \
    \
    # Spool, log and lock directories. Owned by postgres because both run
    # modes execute pgBackRest as that user; 0750 keeps WAL spooled for async
    # archiving away from other users in the container.
    mkdir -p /var/spool/pgbackrest /var/log/pgbackrest /tmp/pgbackrest; \
    chown postgres:postgres /var/spool/pgbackrest /var/log/pgbackrest /tmp/pgbackrest; \
    chmod 0750 /var/spool/pgbackrest /var/log/pgbackrest /tmp/pgbackrest; \
    \
    # The versions as the installed binaries report them, for a running
    # container to be checked against the labels.
    mkdir -p /usr/share/cara; \
    { \
      echo "postgres=$(postgres --version | awk '{print $3}')"; \
      echo "pgbackrest=$(pgbackrest version | awk '{print $2}')"; \
      echo "base=postgres:16-trixie"; \
    } > /usr/share/cara/image-versions; \
    cat /usr/share/cara/image-versions

# tini is PID 1 so the postmaster is not. With archive-async, pgBackRest's
# background archiver is reparented to PID 1 when its parent exits; if that is
# the postmaster and the archiver dies abnormally (the repo host or S3 is
# unreachable), the postmaster treats it as a crashed child and restarts the
# whole database. The PoC reproduced exactly that twice, and saw only ordinary
# archive failures once an init process was PID 1.
#
# Done here rather than with Docker's Init option so it holds however the
# container is started: by the Agent, by the smoke test, or by hand.
#
# The official entrypoint is kept: it initializes PGDATA for the default
# `postgres` command and passes any other command, such as `pgbackrest server`
# for the repo host, straight through without touching PGDATA.
ENTRYPOINT ["tini", "--", "docker-entrypoint.sh"]
CMD ["postgres"]
