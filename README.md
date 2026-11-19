# S3 Object Backup

[English](README.en.md) · [部署文档](docs/deploy.zh-CN.md) · [Deployment](docs/deploy.en.md) · [镜像下载](https://github.com/guohai163/s3-obj-backup/releases)

S3 Object Backup 是一个可自托管的对象存储备份工具。通过同一套 S3 接口连接 Cloudflare R2 和 MinIO，可配置多套凭证以及每套凭证下的多个存储桶。

- **增量备份**支持整桶和目录到目录，保留对象的相对路径。后续运行复制新增、变化或目标端缺失的对象，覆盖目标端同名对象。默认保留源对象，源端删除后也保留目标端副本。
- **移动模式**在目标对象通过 SHA-256 回读校验后复核并删除源对象。
- **目录清理**支持删除目录内全部对象、清理早于固定时间的对象、按天数滚动保留。手动清理提供待删清单和执行确认。
- **对象索引**将文件清单存入 PostgreSQL。可跨桶搜索对象键、浏览目录，并从目录浏览器选择任务范围。
- **Web 控制台**提供管理员和只读账号、简体中文和英文、亮色和暗色。每个任务可设置 Cron 和时区，执行记录包含进度与失败原因。

## 快速启动

```sh
git clone https://github.com/guohai163/s3-obj-backup.git
cd s3-obj-backup
cp .env.example .env
```

编辑 `.env`，设置 `ADMIN_PASSWORD`、`POSTGRES_PASSWORD` 和 `APP_ENCRYPTION_KEY`，然后启动。

```sh
docker compose up -d
```

在本机打开 [Web 控制台](http://127.0.0.1:8080)，使用 `.env` 配置的账号登录。先添加连接和桶，再创建并运行一个对象索引任务。可用外部 PostgreSQL 替代双容器部署，应用会自动迁移表结构。[查看完整部署说明](docs/deploy.zh-CN.md)

启用移动或定时清理前，安排相关目录在整个任务期间暂停写入。R2 及部分 MinIO 版本的 S3 删除操作无法保证原子条件删除。[Cloudflare R2 S3 兼容性](https://developers.cloudflare.com/r2/api/s3/api/)

## 开发

准备 PostgreSQL，配置 `.env` 中的 `DATABASE_URL`，然后运行 `go run ./cmd/s3-obj-backup`。需要 Go 1.25 或更新版本。GitHub Actions 会运行 Go 测试、检查前端脚本并构建容器，版本标签会发布 Linux amd64 和 arm64 镜像。
