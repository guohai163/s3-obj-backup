# 部署 S3 Object Backup

[English](deploy.en.md) · [返回 README](../README.md)

## 应用和 PostgreSQL 双容器

1. 在仓库根目录把 `.env.example` 复制为 `.env`。登录密码至少 12 字节。用 `openssl rand -hex 24` 分别生成登录密码和数据库密码，用 `openssl rand -base64 32` 生成 `APP_ENCRYPTION_KEY`。
2. 执行 `docker compose up -d`。应用默认绑定本机 `127.0.0.1:8080`，数据库数据保存在 Docker 卷 `pgdata`。
3. 打开本机控制台。需要对外访问时，可在应用前配置 HTTPS 反向代理，并设置 `COOKIE_SECURE=true`。代理与应用分属不同主机时，根据网络环境设置 `APP_BIND` 和访问控制。

通过 `APP_IMAGE` 指定版本，例如 `ghcr.io/guohai163/s3-obj-backup:v0.1.0`。从源码构建时，执行 `docker compose up -d --build`。

## 外部 PostgreSQL

准备一个数据库或专用 schema，授予应用账号连接、创建表及索引的权限。将连接串填入 `.env` 的 `DATABASE_URL`，并保留登录和加密配置。应用使用 PostgreSQL 会话锁串行执行任务，连接需直连数据库或使用 session pooling。连接池至少需要 4 个连接。

```sh
docker compose -f compose.external.yaml up -d
```

连接串中的用户名和密码如含 URI 保留字符，需要百分号编码。远程数据库建议使用 `sslmode=verify-full`，并在数据库使用私有 CA 时为容器提供 CA 证书。

## 公开镜像

版本标签会把 Linux amd64、arm64 镜像发布至 GHCR，并在 GitHub Releases 提供公开镜像压缩包与 SHA-256 校验文件。GHCR 包首次发布默认为私有。仓库所有者可在包设置的 **Change visibility** 中设为 **Public**，此后可以匿名拉取。[GitHub Container registry 文档](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

也可从 [GitHub Releases](https://github.com/guohai163/s3-obj-backup/releases) 下载对应架构的压缩包和 `SHA256SUMS`。用 `sha256sum --ignore-missing -c SHA256SUMS` 校验后导入本地 Docker。

```sh
docker load -i s3-obj-backup-linux-amd64.tar.gz
docker pull postgres:17-alpine
docker compose up -d --pull never
```

使用外部数据库时以 `docker compose -f compose.external.yaml up -d --pull never` 启动。

## 配置存储和任务

| 服务 | S3 端点 | Region | 寻址 |
| --- | --- | --- | --- |
| Cloudflare R2 | `https://ACCOUNT_ID.r2.cloudflarestorage.com` | `auto` | 路径式 |
| MinIO | 应用容器能访问的 MinIO S3 API 地址，例如 `http://minio:9000` | 服务器配置的 Region，常用 `us-east-1` | 路径式 |

桶级凭证无法发现存储桶时，在 Web 中手动填写桶名。索引需要 `ListBucket`，备份源需要 `ListBucket` 和 `GetObject`，目标需要 `PutObject`、`GetObject` 及中止分段上传权限。移动和清理另需在被删除的桶授予 `DeleteObject`。R2 连接使用 S3 Access Key ID 和 Secret Access Key。[R2 S3 凭证文档](https://developers.cloudflare.com/r2/api/s3/tokens/)

- 定时表达式使用五段 Cron 和独立的 IANA 时区。例 `0 3 * * *` 配合 `Asia/Shanghai` 表示北京时间每天 03:00。任务串行运行，错过计划启动分钟的定时移动或清理会记录失败。普通备份和索引会合并错过的调度。
- 目录按 S3 对象键前缀处理，末尾自动补 `/`。清理包含子目录并要求非空目录。固定时间清理使用 S3 的 `LastModified` 严格早于截止时间，滚动保留按 24 小时计算。
- 手动清理预览有效期为 1 小时，按预览清单执行一次。移动和清理都需要在整个执行期间暂停相关目录的写入。请用同一个规范 S3 端点描述同一物理桶，并使写入任务的目标目录互不重叠。
- 每个索引任务扫描一个桶。只有完整扫描成功后才发布新的索引，索引时间显示在对象浏览器中。超大对象清单的大小和搜索延迟取决于 PostgreSQL 资源及对象键分布。
- 备份复制当前对象数据及常见 HTTP 元数据和自定义元数据。桶配置、对象标签、ACL、历史版本和保留策略均需在存储侧管理。备份走应用所在主机的数据链路，大文件使用分段上传，移动还会下载目标副本进行校验。

## 账号和运维

`ADMIN_USER` 和 `ADMIN_PASSWORD` 配置管理员。可选的 `VIEWER_USER` 和 `VIEWER_PASSWORD` 配置只读账号，能查看连接、索引、任务和运行结果。修改登录配置后重启会让旧会话失效。S3 密钥在数据库内使用 AES-256-GCM 加密。备份数据库时一并保管 `APP_ENCRYPTION_KEY`，恢复时使用同一个密钥。

用 `docker compose logs app` 查看应用日志。`/healthz` 检查应用与 PostgreSQL 的连通性。
