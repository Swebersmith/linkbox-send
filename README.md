# LinkBox Send

一个适合单台 Ubuntu VPS 的自托管大文件传输系统。React + TypeScript + Vite + Tailwind CSS，Go REST API，SQLite WAL，文件保存在 VPS 本地磁盘。通过 **32 MiB 客户端分片**适配 Cloudflare Proxy，支持刷新恢复、暂停、重试、密码分享和 HTTP Range 下载。

这份仓库包含实际前后端、数据库迁移、Docker Compose、Nginx、测试、备份和恢复脚本。所有核心接口均连接真实数据库和文件系统。Direct Upload 是后续功能，V1 不提供伪造的 direct token 接口。

## Ubuntu 一行安装

先将域名的 A 记录指向 VPS，首次申请证书时将 Cloudflare 记录设为 **DNS Only / 灰云**，并开放入站 TCP 80、443。在全新 Ubuntu VPS 上执行以下一行，替换域名和邮箱：

```bash
curl -fsSL https://raw.githubusercontent.com/Swebersmith/linkbox-send/main/scripts/bootstrap.sh | sudo bash -s -- --domain send.example.com --email you@example.com
```

脚本从本仓库克隆到 `/opt/linkbox-send`，安装 Docker Engine、Compose、Nginx、Certbot，生成随机管理员密码与 Session Secret，构建并启动容器，申请 Let's Encrypt 证书，配置 HTTPS 和自动续期。完成时会在终端显示初始管理员密码；请立即保存。应用数据在 `/opt/linkbox-send/data`，配置在仅 root 可读的 `/opt/linkbox-send/.env`。重复运行会保留这两者，不会重置管理员密码。

安装成功后访问 `https://send.example.com`（换成你的域名），把 Cloudflare DNS 改为 **Proxy ON / 橙云**，SSL/TLS 设为 **Full (strict)**。保留 80 端口供证书续期使用；Cloudflare 的挑战规则不能拦截 `/.well-known/acme-challenge/`。如果证书签发失败，检查域名解析、VPS/云防火墙和 80 端口，修复后重跑同一命令。更新和备份仍按下文流程进行。执行远程脚本前可先查看 [bootstrap.sh](scripts/bootstrap.sh) 和 [install.sh](scripts/install.sh)。

无需公网部署时，可用下文的本机 Docker 快速体验方式。

## 功能

- 多文件队列；默认 4 个并发分片，可选择 1 / 2 / 4 / 6 / 8；每片失败自动重试 3 次，间隔 1 / 2 / 4 秒。
- 上传显示总进度、已上传字节、即时 / 平均速度、ETA、失败分片和状态。
- LocalStorage 只保存当前用户的上传任务 metadata。重新选择同名、同大小、同 lastModified 文件后查询服务端并续传；认证 token 不进入浏览器存储。
- 每片可携带 SHA-256；临时文件落盘、fsync、atomic rename 后写 metadata；最终合并流式读取并重新校验所有分片，计算整文件 SHA-256。
- 合并异步执行，`complete` 返回 202，前端轮询。大文件整理不会依赖单个长 HTTP 请求。
- 下载流式支持单个 HTTP byte range、206 / 416、UTF-8 Content-Disposition、HEAD、`Cache-Control: private, no-store`。
- 256-bit 安全随机分享 token；可设 1 小时 / 1 天 / 7 天 / 30 天 / 永久、密码和最大下载次数；支持撤销、复制和管理分享。
- Admin / User、初次启动管理员引导、管理员创建账号、修改密码；bcrypt cost 12，不保存明文密码。
- HttpOnly + Secure + SameSite=Lax Cookie，会话哈希存库；来源检查、CSRF、参数化 SQL、owner 隔离、登录限速、密码计算和分片并发上限。
- Dashboard、搜索 / 排序 / 分页文件列表、SHA-256、存储和 UTC 当日流量。
- 文件删除采用可重试 tombstone：先标记 deleting，删除文件系统成功后才删除 metadata。
- 每 5 分钟自动清理无活动超过 24 小时的上传、过期访问授权、过期会话和定时删除文件；启动时也执行清理。流量日志保留 90 天。

## 架构与存储

```text
Browser
  │ HTTPS · 每个 PUT 对应一个 32 MiB 分片
Cloudflare Proxy (send.example.com)
  │ Full (strict) TLS
Host Nginx :443
  │ localhost:8080
Docker Nginx :8080 ── React 静态文件 / SPA history fallback
  │ 内部 Docker 网络
Go backend :8080
  ├── SQLite WAL /data/database/linkbox.db
  ├── 分片       /data/chunks/{upload_id}/{index}.part
  └── 文件       /data/files/{file_id}/payload
```

UUID 完全由后端生成；用户提供的文件名只保存在数据库和安全编码后的下载响应头中。文件内容绝不存入数据库。Go `os.Root` 将路径操作限制在配置的存储目录内。后端仅部署 **一个实例**，上传锁和资源限制是进程内的；不要横向扩容、在同一数据目录上启动多个 backend，或将数据库放到 NFS。

SQLite 使用 WAL、`busy_timeout=10000`、外键和 `synchronous=FULL`；迁移启动时按文件名顺序在事务中执行。数据表包括 users、sessions、files、uploads、upload_chunks、shares、share_grants、download_logs、upload_logs、settings。

创建上传时预留 `2 × 文件大小` 的容量，并扣除其他上传的剩余预留，因为合并时分片和最终文件会暂时共存。每片与合并开始时还会检查实际剩余空间。`/data/database`、`/data/files`、`/data/chunks` 应位于**同一本地文件系统**，不要分别挂载不同磁盘。

## Ubuntu VPS 要求

- Ubuntu 22.04 / 24.04 LTS 或兼容系统，amd64 / arm64。
- 建议至少 2 vCPU、2 GiB RAM；存储容量至少为最大并行上传的两倍，加已有文件和默认 2 GiB 安全预留。
- 一行安装需要 sudo 和 apt；脚本会安装 Docker Engine + Compose plugin、Nginx、curl、OpenSSL、Certbot。手动部署需自行安装这些依赖。Python 3 用于恢复 / Cloudflare CIDR 校验脚本。
- 一个域名和 HTTPS 证书。Cloudflare Origin CA 可用于橙云源站；仅用于源站验证，浏览器直访会不信任这种证书。
- 对外只开放所需 SSH 和 HTTPS，后端无宿主机公开端口；应用 HTTP 端口默认只监听 127.0.0.1。

## 安装 Docker

优先遵循 [Docker 官方 Ubuntu 安装文档](https://docs.docker.com/engine/install/ubuntu/)。以下使用官方 apt 仓库：

```bash
sudo apt update
sudo apt install -y ca-certificates curl nginx openssl python3
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt update
sudo apt install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo docker version
sudo docker compose version
```

后续命令需以能够访问 Docker 的账号执行（或者加 `sudo`）。Docker 用户组相当于主机管理员权限。

## 生产部署

将本仓库放在 `/opt/linkbox-send` 等固定目录，然后：

```bash
cd /opt/linkbox-send
cp .env.example .env
chmod 600 .env
openssl rand -hex 32
nano .env
```

至少填写 `APP_URL=https://send.example.com`、`ADMIN_USERNAME`、12–72 字节的 `ADMIN_PASSWORD`、刚生成的 `SESSION_SECRET`。不要使用默认示例密码，不要提交 `.env`。用户名为 3–64 位英文字母、数字、下划线、连字符或点。中文密码按 UTF-8 **字节数**限制为 12–72。

```bash
docker compose config --quiet
docker compose up -d --build
docker compose ps
curl --fail http://127.0.0.1:8080/health
docker compose logs --tail=100 backend
```

`data-init` 会创建并设置 `./data` 所有权为 UID/GID 10001；该一次性服务退出为 0 是正常现象。后端容器以非 root 账号运行，应用和 Nginx 根文件系统只读，运行时临时目录用 tmpfs。实际文件与数据库全部放在 **`./data:/data` bind mount**。`docker compose down` 不会删除这些文件，再次 `up -d` 会重新打开已有数据库。管理员环境变量只在用户表为空时使用，修改环境变量不会重置现有密码。

配置宿主机 TLS：

```bash
sudo install -d -m 700 /etc/ssl/linkbox
# 将有效证书和私钥分别放到 origin.pem、origin.key，私钥 chmod 600。
sudo cp deploy/nginx/host-tls.conf.example /etc/nginx/sites-available/linkbox-send
sudo nano /etc/nginx/sites-available/linkbox-send
sudo ln -s /etc/nginx/sites-available/linkbox-send /etc/nginx/sites-enabled/linkbox-send
sudo nginx -t
sudo systemctl reload nginx
```

修改模板中的域名、证书路径；如果更改 `APP_PORT`，同步修改 `proxy_pass`。默认外部访问地址是 **`https://send.example.com`**（替换为实际域名）。生产安全 Cookie 必须通过 HTTPS 访问。

### 本机 Docker 快速体验

仍先创建 `.env` 并填好管理员密码、Session Secret，然后：

```bash
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

访问 **http://localhost:8080**。此覆盖文件仅在 localhost 开启 HTTP Cookie；生产不要使用。请使用 localhost，不要混用 127.0.0.1，否则 Origin 不匹配。

## Cloudflare 与 DNS

1. 添加 `A send` → VPS IPv4；如果真正配置了 IPv6，再添加 AAAA。
2. `send.example.com` 设置 **Proxy ON / 橙云**。
3. SSL/TLS 模式使用 **Full (strict)**，源站 Nginx 安装匹配域名的有效证书。不要用 Flexible。
4. 添加 Cache Rule：主机名等于 `send.example.com` 且路径以 `/api/` 或 `/s/` 开头时 **Bypass cache**。不要用 Cache Everything 覆盖私有文件响应。
5. 不要对上传 API 启用交互式挑战；WAF / Bot 规则应允许已登录用户的大量分片 PUT。保留正常防护，按实际误拦记录调整。
6. 配置可信客户端 IP，参见 [Cloudflare 操作说明](docs/cloudflare.md)。

Cloudflare 官方文档列出 Free / Pro 单请求上传上限为 100 MB。默认分片 **33,554,432 bytes（32 MiB）**；16 / 32 / 64 MiB 都低于该默认上限。每个分片是独立 HTTP 请求，100 GiB 文件不会变成一个 100 GiB 的请求。Zone 的 Maximum Upload Size 可以被管理员调得更低，请核对实际配置。[官方 413 文档](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/4xx-client-error/error-413/)

分片解决请求体大小限制，不能消除网络和代理超时。超慢连接可以切换 16 MiB；前端会重试失败片，大文件合并则通过 202 + 轮询避开长请求。[Cloudflare 连接限制](https://developers.cloudflare.com/fundamentals/reference/connection-limits/)

Cloudflare 仅作 DNS / CDN / reverse proxy。**没有 Worker 转发文件内容**。部署前也应核对你的套餐和服务条款是否适合预期的文件分发流量。

未来如启用 `upload.example.com`，设置 **DNS Only / 灰云**，独立 HTTPS 直连 VPS。Direct Upload 尚未实现；短时、一次性、默认 10 分钟的 token 契约见 [设计预留](docs/direct-upload.md)。

## Nginx 配置

- Docker Nginx 完整模板：`deploy/nginx/default.conf.template`。
- 主配置及隐私日志格式：`deploy/nginx/nginx.conf`。
- 宿主机 TLS 入口：`deploy/nginx/host-tls.conf.example`。
- 默认 `/api/uploads` 的 `client_max_body_size` 展开为 **40m**；`proxy_request_buffering off`、`proxy_buffering off`，读写超时 1800 秒。
- 下载禁用代理缓冲和临时响应文件，直接流式传输；SPA 路由回退到 index.html。
- 共享 token 出现在 URL，日志不记录 URL / Cookie / 请求体 / Referer。上游负载均衡器也应遵循这一规则。
- **64 MiB 分片**：`UPLOAD_CHUNK_SIZE=67108864`，`NGINX_UPLOAD_BODY_SIZE=72m`，同时修改宿主机 Nginx `client_max_body_size 72m;`，重启容器并 reload Nginx。16 / 32 MiB 可保持 40m。

## 环境变量

| 变量 | 默认 / 示例 | 说明 |
|---|---|---|
| APP_URL | https://send.example.com | 浏览器访问的唯一 origin，不含尾部路径 |
| APP_PORT / APP_BIND | 8080 / 127.0.0.1 | 宿主机端口和绑定地址 |
| ADMIN_USERNAME / ADMIN_PASSWORD | admin / 必填 | 空数据库时初始化管理员 |
| SESSION_SECRET | 必填 | 至少 32 个随机字符，推荐 openssl rand -hex 32 |
| DATABASE_PATH | /data/database/linkbox.db | SQLite 文件路径 |
| STORAGE_PATH | /data/files | UUID 文件根目录 |
| CHUNK_PATH | /data/chunks | UUID 分片根目录 |
| UPLOAD_CHUNK_SIZE | 33554432 | 只接受 16777216 / 33554432 / 67108864 |
| UPLOAD_CONCURRENCY | 4 | 客户端默认并发 1 / 2 / 4 / 6 / 8 |
| UPLOAD_EXPIRE_HOURS | 24 | 上传无活动过期时间，成功写片会续期 |
| MAX_FILE_SIZE | 107374182400 | 100 GiB，以字节计 |
| STORAGE_RESERVED_BYTES | 2147483648 | 2 GiB 安全预留 |
| MAX_ACTIVE_UPLOADS | 100 | 全局未完成上传数 |
| MAX_UPLOADS_PER_USER | 10 | 每用户未完成上传数 |
| MAX_CHUNK_REQUESTS | 16 | 后端同时处理的分片请求上限 |
| LOG_LEVEL | info | debug / info / warn / error |
| TRUST_PROXY | true | 只从受信任 CIDR 接受 X-Real-IP |
| TRUSTED_PROXY_CIDRS | 172.30.48.0/24 | Docker 私有网络；修改网络时同步修改 |
| COOKIE_SECURE | true | false 仅允许 APP_URL 为 localhost / 127.0.0.1 |
| NGINX_UPLOAD_BODY_SIZE | 40m | 64 MiB 分片时改 72m，且同步所有代理层 |

更改 Session Secret 会使所有现有 Session / 分享密码授权失效，永久分享 token 本身仍保留。不要在线修改已开始上传的 chunkSize；已有任务会使用它创建时保存的大小。改变存储路径或网络网段应停机操作并先备份。

## API 与下载计数语义

接口列表和请求示例见 [docs/api.md](docs/api.md)。所有带 Session 的写请求必须携带 `Origin: APP_URL` 和 `/api/auth/me` 返回的 `X-CSRF-Token`；登录和公共分享验证也校验 Origin。

`maxDownloads` 按**被接受的 GET 下载请求**计数，HTTP Range GET 也算一次，HEAD 和无效 Range 不计数。条件 UPDATE 保证并发不会越过上限。下载被用户中途取消也已经消耗次数；因此需要断点续传或多线程下载器时，请设置较大的上限或 unlimited。过期 / 撤销 / 达到上限返回 410，未知或文件已删除的分享返回 404。密码正确后获得 15 分钟的 HttpOnly 分享授权 Cookie，到期需重新验证。

文件默认由 owner 永久保留，分享过期**不会自动删除 owner 的文件**。可用 `PATCH /api/files/{id}` 将 `retainOwner=false` 并指定未来 `deleteAt`；清理器到期删除实际文件和 metadata。此高级保留策略目前通过 API 设置。

## 备份

```bash
chmod +x scripts/*.sh
sudo ./scripts/backup.sh /srv/linkbox-backups
```

备份脚本在退出时恢复原先运行中的 backend。它先停止后端形成文件系统一致性窗口，再通过 SQLite **`.backup`** 生成数据库快照，执行 `integrity_check`，归档 `/data/files`，并生成 SHA256SUMS。**不会直接复制正在写入的主 database file 来忽略 WAL**。

备份包包含 linkbox.db、files.tar.gz、manifest.txt、SHA256SUMS。备份期间上传 / 下载会中断，请在低峰运行；大数据量备份的暂停时间与归档耗时有关。临时 chunks 不备份。备份内含用户密码哈希和分享链接能力，限制目录权限，并加密后存放异机。

另外在安全位置保存 `.env`、TLS 配置与证书、当前代码版本。不要把 `.env` 推到 Git。备份本身不是加密备份，脚本不会自动上传到外部服务。定期在独立目录恢复演练。

## 恢复

先准备相同版本代码、Docker 镜像和 `.env`，再执行：

```bash
sudo ./scripts/restore.sh /srv/linkbox-backups/linkbox-20260925T120000Z
# 若已有 ./data，明确保留旧数据到 rollback 目录再替换：
sudo ./scripts/restore.sh /srv/linkbox-backups/linkbox-20260925T120000Z --replace
docker compose ps
curl --fail http://127.0.0.1:8080/health
```

脚本校验 SHA256SUMS、归档路径和类型，拒绝路径穿越、链接及设备文件；已有 data 仅在 `--replace` 下移至带时间戳的目录，**不删除旧数据**。SQLite 恢复后执行完整性检查并清除旧 Session、分享密码授权和未完成上传 metadata；文件与分享保留。需要重新登录，未完成任务需重新上传。若恢复失败，保留现场并将旧 data 目录移回后再启动，不要直接删除 rollback。

## 升级

1. 记录当前版本，运行备份，确认备份校验通过。
2. 拉取新代码，比较 `.env.example` 的新增变量；保留原来的 `.env` 和 `./data`。
3. `docker compose build --pull`，然后 `docker compose up -d`。
4. 检查健康状态、日志，做一次上传和下载校验；迁移会在启动时自动执行。
5. 需要回滚 schema 时，使用匹配旧代码版本的完整备份恢复，不能假设旧二进制兼容新的迁移。

## 本地开发与测试

Go 1.27.1、Node.js 24。Linux/macOS 示例：

```bash
cp .env.example .env
# 填好密码和 SESSION_SECRET；可使用下面独立开发环境。
cd backend
go mod download
go test ./...
go vet ./...
APP_URL=http://localhost:5173 COOKIE_SECURE=false TRUST_PROXY=false \
ADMIN_USERNAME=admin ADMIN_PASSWORD='replace-with-your-password' \
SESSION_SECRET="$(openssl rand -hex 32)" \
DATABASE_PATH=../data/database/linkbox.db STORAGE_PATH=../data/files CHUNK_PATH=../data/chunks \
go run ./cmd/server
```

在另一个终端：

```bash
cd frontend
npm ci
npm run dev
npm run build
```

访问 http://localhost:5173。Vite 代理 `/api` 到 127.0.0.1:8080，Origin 与开发 APP_URL 一致。环境变量由 shell / Compose 注入，Go 不会自动读取根目录 `.env`。

```bash
./scripts/generate-test-file.sh ./test-1g.bin 1024
# 上传后下载：
sha256sum ./test-1g.bin ./downloaded-test-1g.bin
```

后端测试使用临时 SQLite 和临时磁盘文件，测试用小分片覆盖 init、chunk、resume（进程重开）、complete、missing chunk、Range、expired/password/limited share、path traversal、取消、删除、owner 隔离、CSRF、磁盘保护和清理。Linux CI 还运行 `go test -race`、前端构建、Docker 构建及 Nginx 健康检查。生产负载和 100 GiB 文件的实际传输耗时应在目标 VPS 上验收。

## 故障排查

| 现象 | 检查 |
|---|---|
| 登录成功后仍未登录 | 确认 HTTPS、Cookie Secure、APP_URL、访问主机名及 Origin 完全一致；本地使用 dev override |
| 403 invalid request origin | 访问域名与 APP_URL 不同，或脚本未带 Origin；不支持未配置的备用域名 |
| 413 | 检查 Cloudflare Zone Maximum Upload Size，宿主机和 Docker Nginx body size；64 MiB 需要两层 72m |
| 409 missingChunks | 重新 GET 上传状态，补传返回列表中的分片，再 complete |
| 422 SHA mismatch | 分片传输 / 源文件内容有变，重新选择原文件重试；不要用不同文件冒充同一任务 |
| 429 | 登录 / 密码验证限速或并发 / 上传数上限；暂停等待，降低并发，不要无限即时重试 |
| 507 | 空间不足，计算已有预留和合并副本；取消闲置任务或清理文件，不能只看文件大小 |
| 合并中断 | 重启会将 merging 恢复为 uploading；原分片保留，可重新 complete，损坏片自动变为 missing |
| 410 | 分享过期、撤销、下载次数耗尽，或上传任务已过期 |
| 524 / 断网 | 分片失败会重试；必要时减小分片到 16 MiB；合并本身应返回 202，查询状态即可 |
| permission denied | 检查 ./data UID/GID 10001；恢复脚本会修复所有权。不要用 chmod 777 |
| 数据库 locked | 不要运行多实例或外部写 SQLite；确认本地磁盘、WAL 支持和合理 I/O 负载 |
| Docker 子网冲突 | 同步更改 Compose IPAM、frontend 固定地址、Nginx trusted gateway 和 TRUSTED_PROXY_CIDRS |
| 所有登录看起来同一 IP | 正确安装 Cloudflare real-IP 配置，检查多层代理转发；不要信任任意访客 header |
| 前端刷新任务缺失 | 浏览器本地数据被清理或使用另一浏览器；`GET /api/uploads` 可查看服务端任务，过期前仍可 API 续传 |

## 目录

```text
backend/
  cmd/server/main.go
  internal/app/
    app.go, auth.go, config.go, db.go, security.go
    uploads.go, shares.go, files.go, cleanup.go
    disk_*.go, sync_*.go, app_test.go
    migrations/001_initial.sql
  Dockerfile, go.mod, go.sum
frontend/
  src/App.tsx, upload.ts, api.ts, styles.css, main.tsx
  public/favicon.svg
  Dockerfile, package.json, package-lock.json, vite.config.ts, tsconfig.json
deploy/nginx/
  nginx.conf, default.conf.template, host-tls.conf.example
scripts/
  backup.sh, restore.sh, generate-test-file.sh, cloudflare-realip.sh
docs/
  api.md, cloudflare.md, direct-upload.md
.github/workflows/ci.yml
.env.example
docker-compose.yml
docker-compose.dev.yml
README.md
```

## 后续版本范围

Direct Upload 一次性令牌、S3 / 多节点存储、组织 / 配额管理、SSO / MFA、邀请注册、整文件端到端加密、恶意文件扫描、自动删除策略 UI、上传完成通知等尚未实现。V1 聚焦单机、多用户隔离、大文件分片和可恢复传输；现有分享不是端到端加密，服务器管理员可读取本地文件。
