# S3 Object Backup

[简体中文](README.md) · [Deployment](docs/deploy.en.md) · [Image downloads](https://github.com/guohai163/s3-obj-backup/releases)

S3 Object Backup is a self-hosted tool for backing up object storage. It connects to Cloudflare R2 and MinIO through the S3 API and supports multiple sets of credentials with multiple buckets per connection.

- **Incremental backup** copies whole buckets or maps source folders to destination folders, preserving relative keys. Later runs copy new, changed, or missing objects and overwrite destination objects with the same key. The source is retained by default. Deleting a source object leaves its destination copy in place.
- **Move mode** reads back the destination to verify its SHA-256, then rechecks and removes the source.
- **Folder cleanup** removes every object in a folder, removes objects older than a cutoff, or keeps the last N days. Manual cleanup presents a preview and asks for confirmation.
- **Object indexing** stores object listings in PostgreSQL. Search keys across buckets, browse folders, and select task scopes from the folder browser.
- **Web console** supports administrator and read-only accounts, Simplified Chinese and English, and light and dark themes. Each task has its own Cron expression and timezone. Run history shows progress and errors.

## Quick start

```sh
git clone https://github.com/guohai163/s3-obj-backup.git
cd s3-obj-backup
cp .env.example .env
```

Set `ADMIN_PASSWORD`, `POSTGRES_PASSWORD`, and `APP_ENCRYPTION_KEY` in `.env`, then start the services.

```sh
docker compose up -d
```

Open the [local console](http://127.0.0.1:8080) and sign in with the configured credentials. Add a connection and a bucket, then create and run an indexing task. The application also supports an external PostgreSQL instance and applies schema migrations automatically. [Deployment guide](docs/deploy.en.md)

For move or scheduled cleanup, pause writers to the affected prefixes throughout each run. R2 and some MinIO releases lack a documented atomic conditional delete guarantee. [Cloudflare R2 S3 compatibility](https://developers.cloudflare.com/r2/api/s3/api/)

## Development

Install Go 1.25 or later, configure PostgreSQL through `DATABASE_URL` in `.env`, and run `go run ./cmd/s3-obj-backup`. GitHub Actions tests Go code, checks the frontend scripts, and builds the container. Version tags publish Linux amd64 and arm64 images.
