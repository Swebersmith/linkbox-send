# Cloudflare 部署检查

## DNS 与 TLS

`send.example.com` 的 A / AAAA 指向 VPS，并启用 Proxy ON。使用 Full (strict) 验证宿主机 Nginx 的有效证书。Origin CA 证书只给 Cloudflare 到源站这段连接使用；如需浏览器直接访问该域名，请配置公开可信证书。

使用默认 32 MiB 分片，Zone Maximum Upload Size 至少高于 33,554,432 字节。对于 64 MiB 配置，需高于 67,108,864 字节，并同时设置两层 Nginx 为 72m。大文件会分成多个 HTTP 请求，后台合并返回 202 并通过短 GET 轮询。

## 缓存

设置 Cache Rule 为 `/api/*` 和 `/s/*` Bypass。下载响应还会设置 `Cache-Control: private, no-store`。静态 `/assets/*` 使用内容哈希，可以长期缓存。不要用 Cache Everything 强行覆盖 API 响应，不要在 Worker 中缓存或转发文件内容。

## 可信客户端 IP

后端不直接相信浏览器传来的 `CF-Connecting-IP` 或 `X-Forwarded-For`。信任链是：

1. 宿主机 Nginx 只对 Cloudflare 官方 CIDR 采用 CF-Connecting-IP，其他来源使用实际 remote_addr。
2. 宿主机 Nginx 覆盖 X-Real-IP。
3. 容器 Nginx 只信任 Docker host gateway `172.30.48.1`，再覆盖发给后端的 X-Real-IP。
4. 后端只接受 TRUSTED_PROXY_CIDRS 中连接的 X-Real-IP。backend 没有宿主机公开端口。

```bash
./scripts/cloudflare-realip.sh > /tmp/cloudflare-realip.conf
cat /tmp/cloudflare-realip.conf
sudo install -m 644 /tmp/cloudflare-realip.conf /etc/nginx/snippets/cloudflare-realip.conf
# 取消 host-tls.conf 中 include 的注释
sudo nginx -t && sudo systemctl reload nginx
```

脚本只下载 Cloudflare 官方 v4/v6 CIDR 文本，使用 Python ipaddress 校验，不执行下载代码。应定期与官方地址变化同步。可以在 VPS / 云安全组只允许 Cloudflare CIDR 访问 443，SSH 独立限制到管理地址。不要把所有来源配置为 `set_real_ip_from 0.0.0.0/0`。

未配置该信任链时仍可传输，但多个用户会共享某个代理 IP 的登录速率限制。使用受信任源地址链可以防止攻击者随意伪造 header 绕过限制。

## 未来直连域名

`upload.example.com` 设置 DNS Only，使用独立公网 HTTPS 证书；只接受短期一次性 upload token。UI 与登录仍在 send 域名。契约见 [direct-upload.md](direct-upload.md)，V1 只实现分片模式。

## 官方参考

- [单请求上传大小 / 413](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/4xx-client-error/error-413/)
- [连接与超时限制](https://developers.cloudflare.com/fundamentals/reference/connection-limits/)
- [Cloudflare IP ranges](https://www.cloudflare.com/ips/)
- [Full (strict)](https://developers.cloudflare.com/ssl/origin-configuration/ssl-modes/full-strict/)
- [恢复访客 IP](https://developers.cloudflare.com/support/troubleshooting/restoring-visitor-ips/restoring-original-visitor-ips/)
