# cara-postgres:16

PostgreSQL 16 with pgBackRest and `tini`. Users run it for any database whose Managed volume uses the `PostgresDifferential` backup strategy, and each Node's pgBackRest repo host runs from it as well.

Current versions: base `postgres:16-trixie`, pgBackRest `2.59.3-1.pgdg13+1`.

```bash
make build-cara-postgres-image
```

The target builds `ghcr.io/nycu-sdc/cara-postgres:16` for the host architecture and prints the image ID and the recorded versions.

```bash
make test-cara-postgres-image
```

The smoke test builds the image and checks the contract below against the host's Docker in about a minute, with no repo host or S3: PID 1, archiving off by default, recorded versions, UID/GID, certificate permissions, a full and a differential backup with `compress-type=zst` restored and queried, the async archiving crash `tini` prevents (with a control run without `tini` that must reproduce it), and repo-host mode. CI runs it on amd64 and arm64 when the image or the test changes. zstd is the compression this image is tested with; use it in the configuration the Agent mounts.

## Publishing

The `cara-postgres image` workflow publishes `ghcr.io/nycu-sdc/cara-postgres:16` for amd64 and arm64 when the image or the workflow changes on `main`, or when it is run by hand. Each architecture pushes the image its own smoke test passed, so nothing is published without passing it. The package must be public, because Nodes pull with no credentials; the workflow fails if it is not.

Each run also publishes `16-<run>.<attempt>`, which is never reused. `16-<run>.<attempt>-amd64` and `-arm64` are the single-architecture images it is made from.

## Contract

Cara's validation, placement and backup code depend on the values below. They can change, but a change must update every consumer in the same change and keep the smoke test's check of that value passing.

| Item | Value | Consumers |
|---|---|---|
| Image reference | `ghcr.io/nycu-sdc/cara-postgres:16` | apply-time validation of `PostgresDifferential` |
| PID 1 | `tini`; entrypoint `tini -- docker-entrypoint.sh`, default command `postgres` | none directly; protects the database from async archiving failures |
| PGDATA | `/var/lib/postgresql/data` | validation of the volume's mount path; placement |
| postgres user | UID and GID 999 | Agent, which chowns certificate files and mounted configuration to it |
| pgBackRest config | `/etc/pgbackrest/pgbackrest.conf`, mounted read-only; the image ships none | Agent |
| Certificates | `/etc/cara/pgbackrest/tls/` with `ca.crt`, `client.crt`, `client.key`, mounted read-only | Agent |
| Spool, log, lock | `/var/spool/pgbackrest`, `/var/log/pgbackrest`, `/tmp/pgbackrest`; postgres-owned, 0750 | pgBackRest |
| Recorded versions | labels `dev.cara.postgres.version`, `dev.cara.pgbackrest.version`, `dev.cara.base.image`; file `/usr/share/cara/image-versions` | repo host startup check, operators |

### Two run modes

The two modes share the image, not the start-up path.

| | PostgreSQL mode | Repo-host mode |
|---|---|---|
| Command | `postgres` (default) | `pgbackrest server` |
| User | starts as root; the official entrypoint drops to postgres | started as `999:999` by whoever creates the container |
| PGDATA | initialized on first start by the official entrypoint | never touched: the entrypoint passes non-`postgres` commands straight through |
| Archiving | off until the Agent's placement enables it, after the stanza exists | not applicable |

Archiving is off by default because turning it on before the stanza exists breaks initialization: the temporary server that initdb starts cannot archive, `pg_ctl` cannot stop it, and the container exits.

Retention, bundling and block-incremental settings (`repo1-retention-full`, `repo1-bundle`, `repo1-block`) are deliberately absent. They take effect only on the side that issues the command, so they belong in the configuration the Agent mounts into the database container.

### One tag is not one content

`:16` moves with each publish, and a Node pulls an image only when it has none, so two Nodes can hold different contents under `:16`, with different PostgreSQL minors or, after a bump, different pgBackRest versions. The labels and the version file exist so this can be checked; the repo host compares client and server pgBackRest versions when it starts. To keep every Node on one content, set the server's `POSTGRES_DIFFERENTIAL_IMAGE` to a `16-<run>.<attempt>` tag; manifests then name that same reference.

## Updating

| What | How |
|---|---|
| PostgreSQL minor | Run the workflow by hand. The base tag `postgres:16-trixie` follows the latest 16.x |
| pgBackRest | Change `PGBACKREST_VERSION` in the Dockerfile to a version in the PGDG `trixie-pgdg` repository for both amd64 and arm64, and merge. Every Node must then pull the new image, because the repo host and the databases must agree |
| Debian release | Change the base tag and the `pgdgNN` suffix of `PGBACKREST_VERSION` together; the package is built per release |
| PostgreSQL major | A new image directory and reference (for example `17/`), not an edit here. PGDATA is not compatible across majors |

Nothing reminds anyone to do these: the repository has no automated dependency updates.
