# Deploy S3 Object Backup

[简体中文](deploy.zh-CN.md) · [Back to README](../README.en.md)

## Application and PostgreSQL containers

1. Copy `.env.example` to `.env` at the repository root. Login passwords require at least 12 bytes. Generate separate login and database passwords with `openssl rand -hex 24`. Generate `APP_ENCRYPTION_KEY` with `openssl rand -base64 32`.
2. Run `docker compose up -d`. The application binds to `127.0.0.1:8080` by default. Docker stores PostgreSQL data in the `pgdata` volume.
3. Open the local console. For external access, configure an HTTPS reverse proxy and set `COOKIE_SECURE=true`. If the proxy runs on another host, set `APP_BIND` and network access controls for that deployment.

Set `APP_IMAGE` to pin a release, for example `ghcr.io/guohai163/s3-obj-backup:v0.1.0`. To build from source, run `docker compose up -d --build`.

## External PostgreSQL

Create a database or dedicated schema, and give the application role permission to connect and create tables and indexes. Set `DATABASE_URL` in `.env` alongside the login and encryption settings. The worker uses a PostgreSQL session lock, so use a direct connection or session pooling. The application pool needs at least four connections.

```sh
docker compose -f compose.external.yaml up -d
```

Percent-encode reserved characters in connection string usernames and passwords. Use `sslmode=verify-full` for a remote database and provide the database CA to the container if it uses a private CA.

## Public images

Version tags publish Linux amd64 and arm64 images to GHCR and add downloadable Docker archives and SHA-256 checksums to GitHub Releases. GHCR defaults to private on first publication. The repository owner can set the package to **Public** through **Change visibility** in its package settings to enable anonymous pulls. [GitHub Container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

You can also download the architecture-specific archive plus `SHA256SUMS` from [GitHub Releases](https://github.com/guohai163/s3-obj-backup/releases). Verify it with `sha256sum --ignore-missing -c SHA256SUMS` and load it locally.

```sh
docker load -i s3-obj-backup-linux-amd64.tar.gz
docker pull postgres:17-alpine
docker compose up -d --pull never
```

For external PostgreSQL, run `docker compose -f compose.external.yaml up -d --pull never`.

## Configure storage and tasks

| Provider | S3 endpoint | Region | Addressing |
| --- | --- | --- | --- |
| Cloudflare R2 | `https://ACCOUNT_ID.r2.cloudflarestorage.com` | `auto` | Path style |
| MinIO | The S3 API URL reachable from the application, such as `http://minio:9000` | Server region, commonly `us-east-1` | Path style |

If bucket discovery is unavailable with bucket-scoped credentials, enter the name directly. Indexing requires `ListBucket`. A backup source requires `ListBucket` and `GetObject`, while its destination requires `PutObject`, `GetObject`, and multipart abort permissions. Move or cleanup also needs `DeleteObject` on the bucket being modified. Use the S3 Access Key ID and Secret Access Key for R2. [R2 credentials documentation](https://developers.cloudflare.com/r2/api/s3/tokens/)

- Schedules use five-field Cron and a separate IANA timezone. For example, `0 3 * * *` with `Asia/Shanghai` runs daily at 03:00 in Shanghai. Tasks run serially. A scheduled move or cleanup fails if it misses its start minute. Regular backups and index tasks coalesce missed occurrences.
- Folders are S3 key prefixes with a trailing slash. Cleanup is recursive and requires a non-empty folder. A cutoff matches S3 `LastModified` timestamps strictly earlier than the selected instant. Rolling retention counts 24-hour days.
- A manual preview can be used once within one hour. Keep writers paused to the affected folders throughout any move or cleanup. Use one canonical S3 endpoint for a physical bucket and keep write destinations for independent tasks disjoint.
- An indexing task scans one bucket. The application publishes a new index only after a complete scan. The object browser displays the last successful index timestamp. Large inventory size and search latency depend on PostgreSQL resources and the distribution of object keys.
- Backups copy current object contents, common HTTP headers, and user metadata. Manage bucket configuration, tags, ACLs, historical versions, and retention settings in the storage provider. Data passes through the application host. Large objects use multipart uploads, and move mode reads the destination again for verification.

## Accounts and operation

Configure the administrator with `ADMIN_USER` and `ADMIN_PASSWORD`. Optionally set `VIEWER_USER` and `VIEWER_PASSWORD` to allow read-only access to connections, inventory, tasks, and run results. Restarting after a change to the login settings invalidates old sessions. S3 credentials are stored with AES-256-GCM encryption. Back up `APP_ENCRYPTION_KEY` alongside the database and use the same key when restoring.

Use `docker compose logs app` for application logs. The `/healthz` endpoint checks PostgreSQL connectivity.
