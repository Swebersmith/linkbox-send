# Direct Upload 后续接口契约

V1 的可靠上传链路是 `/api/uploads` 分片接口。Direct Upload 是明确保留给后续版本的独立传输入口，当前不注册虚假的成功接口，`/api/direct-token` 和 `/api/direct/{token}` 返回 404。

未来 `upload.example.com` 必须设置 DNS Only，通过独立 HTTPS Nginx server 接入同一个 Go 应用和 filesystem storage。不要经过 Worker。管理、登录、分享继续使用 `send.example.com`。

实施契约：

1. `POST /api/direct-token` 在管理域名上校验 Session、CSRF、文件信息、用户限额及磁盘预留，然后生成 256-bit CSPRNG token。只返回一次，数据库仅保存 HMAC 哈希、owner、file metadata、预留容量、过期时间及消费状态。
2. 默认有效期 **10 分钟**，不得签发永久 token。token 只能绑定一个预声明大小的文件。
3. `PUT /api/direct/{token}` 不使用跨域 Session Cookie；使用 token 作为一次性 capability。通过条件 UPDATE 原子地将 issued 改为 consuming，重放返回 410。
4. 先消费 token，再读取 body；流式限制总字节数，边写边计算 SHA-256；失败或中断也不会让 token 再次可用，需要重新申请。成功提交最终文件 metadata 后转 complete。
5. 复用现有 UUID 路径、磁盘准入、文件提交、删除和 cleanup 规则。CORS 仅允许配置的 APP_URL。对 direct URL、token、Cookie 和密码禁止访问日志。
6. 独立 Nginx upload server 可使用 `client_max_body_size 0`，同时后端必须强制声明大小和 MAX_FILE_SIZE，继续关闭所有请求 / 响应缓冲并限制并发。

数据库迁移目录按顺序执行，可用新 migration 增加 `direct_upload_tokens`；现有 `files`、`uploads`、存储目录和认证无须迁移至其他服务。
