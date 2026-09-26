# LinkBox Send REST API

所有 metadata API 返回 JSON。错误统一为 `{ "error": "..." }`，missing chunks 附加 `missingChunks`。返回 ID 为 UUID v4，分享 token 是 32 个安全随机字节的 Base64URL 编码。

写操作带 `Origin`（精确等于 APP_URL），登录后还需 `X-CSRF-Token`。认证 Cookie 由浏览器自动发送。CLI 使用 cookie jar，不要把会话写入 localStorage。

```bash
origin=https://send.example.com
# 示例：用受权限保护的文件传入凭据，不要在公开 shell 历史中写真实密码。
curl -c cookies.txt -H "Origin: $origin" -H 'Content-Type: application/json' \
  --data-binary @login.json "$origin/api/auth/login"
curl -b cookies.txt "$origin/api/auth/me"
# 从响应读取 csrfToken，后续写请求添加 -H "X-CSRF-Token: $csrf"
```

## 认证

| 方法 | 路径 | Body / 结果 |
|---|---|---|
| POST | /api/auth/login | `{username,password}` → `{user,csrfToken}` + Set-Cookie |
| GET | /api/auth/me | `{user:{id,username,role},csrfToken}` |
| POST | /api/auth/logout | 删除当前会话，204 |
| POST | /api/auth/password | `{currentPassword,newPassword}`；删除所有旧会话，204 |
| GET | /api/users | Admin 列出用户 |
| POST | /api/users | Admin `{username,password,role:"user"}`，201 |

密码限制为 12–72 UTF-8 字节。会话默认 7 天，登录轮换 session / CSRF，最多保留每用户 10 个会话。登录限速：每 IP 10 次 / 15 分钟，每用户名 30 次 / 15 分钟，失败和成功请求均计入；只用精确用户名限制会导致分布式枚举，因此同时设 IP 和全局密码计算并发保护。

## 上传

`GET /api/settings` 获取服务器 chunkSize，默认 33554432；客户端不能自行选择不同 chunkSize。

```http
POST /api/uploads
Content-Type: application/json

{"name":"example.zip","size":123456789,"mimeType":"application/zip","chunkSize":33554432}
```

201 响应含 uploadId、chunkSize、totalChunks、uploadedChunks、fileId、fileName、fileSize、status、createdAt、expiresAt。文件过大返回 413，剩余空间扣除保留及上传工作空间不足时返回 507，超过任务数返回 429。

- `GET /api/uploads`：当前用户最近 200 个任务。
- `GET /api/uploads/{uploadId}`：状态与实际已存在的分片索引。轮询 merging 时不会等待合并磁盘锁。
- `PUT /api/uploads/{uploadId}/chunks/{index}`：`Content-Type: application/octet-stream`，可带 `X-Chunk-SHA256`（64 位 hex）。index 从 0 开始，最后一片按声明总长度裁切；成功 204。
- 同一 index 的相同字节可以安全重试；不同内容返回 409，校验不匹配 422。后台会重新计算所有片及整文件哈希。
- `POST /api/uploads/{uploadId}/complete`：缺片 409 + `{missingChunks:[...]}`；完整则立即 202 `{status:"merging",fileId,...}`；已完成重复调用 200。轮询 GET，直到 status=complete。若 status=uploading 且 error 有值，重新同步分片并重试。
- `DELETE /api/uploads/{uploadId}`：删除临时文件和任务 metadata，204。对于已经 complete 的任务，仅删除任务记录，不删除实际 files；实际文件通过 DELETE /api/files 删除。

size=0 文件允许创建，无分片，直接 complete。任务仅接受 owner 的 Session。chunks 不会全量读入服务端 RAM。会话失效时前端需要重新登录，再重新选择文件恢复。

## 文件

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/files?search=&sort=newest&page=1 | 每页 50，`{files,total,page,pageSize}` |
| DELETE | /api/files/{fileId} | 先删除 filesystem，成功后删除 metadata 和 shares |
| PATCH | /api/files/{fileId} | `{retainOwner:false,deleteAt:"2026-12-01T00:00:00Z"}` 设置自动删除 |
| GET | /api/dashboard | 磁盘 total/used/available/reserved、任务、文件及 UTC 今日流量 |
| GET | /api/settings | 非敏感服务端配置 |

排序仅支持白名单 newest / oldest / name / largest / smallest。列表只展示当前 owner。SHA-256 是整文件摘要。流量统计计入被完整接受的分片请求（重传也计）和 HTTP 实际写出的下载 body，进行中的下载在请求结束时记录。

## 分享

```http
POST /api/files/{fileId}/shares
Content-Type: application/json

{"expiresIn":604800,"password":"","maxDownloads":0}
```

201 返回 `{id,token,url}`。expiresIn 允许 3600 / 86400 / 604800 / 2592000 / 0（永久），maxDownloads=0 表示 unlimited。

- `GET /api/files/{fileId}/shares`：owner 管理分享，含 URL、密码保护状态、到期时间和计数。
- `DELETE /api/shares/{shareId}`：owner 撤销分享，204。
- `GET /api/shares/{token}`：公开分享 metadata。未解锁密码时只返回 `{passwordRequired:true}`，不泄露文件名。
- `POST /api/shares/{token}/verify`：`{password:"..."}`；正确时设置 15 分钟 HttpOnly grant Cookie 并返回 metadata；错误 401，速率超限 429。
- `GET /api/download/{token}`：直接 streaming body，支持 `Range: bytes=1000000-`、闭区间和 suffix range；多区间请求因资源保护返回 416。
- `HEAD /api/download/{token}`：相同鉴权与响应头，不发送文件内容、不消耗次数。

每次成功接受的 GET（包括 Range）消耗一次额度；GET 断流仍算已开始下载。计数原子更新，超额 / 过期 / 撤销 410。每份分享的计数独立。原始名字用安全的 RFC 5987 编码进入 Content-Disposition；下载内容强制 attachment + application/octet-stream + nosniff。

## 健康检查

`GET /health` 在数据库可访问时返回 200 `{ "status":"ok" }`，否则 503。Compose 的 backend 和 frontend 都配置 healthcheck。
