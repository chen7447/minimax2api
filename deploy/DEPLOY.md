# MiniMax2API 部署手册（境外 VPS）

目标形态：VPS 上以 systemd 常驻 `127.0.0.1:8080`，前端挂 Cloudflare 域名子域，客户端密钥（`sk-mm-…`）注册进 New API 作为渠道。宿主机在境外，**无需配置 `upstream.proxy`**。

## 0. 包内容

`Minimax2api.bat pack` 产出 `deploy/minimax2api-deploy-linux-amd64.tar.gz`：

```
minimax2api            linux/amd64 二进制（含 -trimpath -ldflags "-s -w"）
dist/                  前端产物（管理台 SPA）
minimax2api.service    systemd 单元
DEPLOY.md              本手册
```

> 依赖：Go 1.27.3+（本机在 `C:\Program Files\Go\bin`；`go.mod` 的 `toolchain` 行会强制拉取该版本）、Node 20+（仅构建前端用）。VPS 本身**零依赖**：纯 Go 标准库静态编译，无 docker、无数据库。

## 1. 上传与安装（VPS，root）

```bash
# 本机（Git Bash）上传
scp deploy/minimax2api-deploy-linux-amd64.tar.gz root@<VPS>:/tmp/

# VPS 上
mkdir -p /opt/minimax2api
tar -xzf /tmp/minimax2api-deploy-linux-amd64.tar.gz -C /opt/minimax2api
chmod +x /opt/minimax2api/minimax2api

# 专用系统用户
useradd -r -s /usr/sbin/nologin minimax2api 2>/dev/null || true
chown -R minimax2api:minimax2api /opt/minimax2api
```

## 2. 初始管理员密钥（600 权限 env 文件）

```bash
cat > /opt/minimax2api/env <<'EOF'
MINIMAX2API_ADMIN_KEY=mm2api-换成你的随机密钥
EOF
chmod 600 /opt/minimax2api/env
chown minimax2api:minimax2api /opt/minimax2api/env
```

> 管理台是**单密钥**登录（没有用户名 / 密码），同一把 key 也能直接当 Bearer 调 `/admin/api/*`：
> `curl -H "Authorization: Bearer mm2api-…" http://127.0.0.1:8080/admin/api/dashboard`
> 该密钥是**初始**密钥，之后改密钥走管理台；改完不必更新 env 文件（env 只在首启无账号时生效）。

## 3. systemd 常驻

```bash
cp /opt/minimax2api/minimax2api.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now minimax2api
curl -s http://127.0.0.1:8080/health     # 期望 {"pool":...,"status":"ok",...}
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/   # 期望 200（管理台 SPA）
```

> ⚠️ `-static dist` 不能省。包里的前端产物在顶层 `dist/`（不是 `frontend/dist`），而默认值是 `frontend/dist`；
> 一旦找不到 index.html，服务会**只提供 API、一条静态路由都不注册**（启动日志打「提示: 未找到前端产物 …」），
> 表现为 `/health` 200 但 `/` 404——浏览器打开域名就是一片 404。2026-10-02 首次部署踩过这个坑。

若 8080 被其他实例占用：改 `minimax2api.service` 里的 `-addr 127.0.0.1:8080`，同时同步第 4 步的反代目标。

## 4. 挂到 Cloudflare 域名（跟你现有 2api 实例保持一致，二选一）

**A. cloudflared Tunnel**（VPS 上已有 tunnel 就走这条）——在 ingress 规则里加：

```yaml
- hostname: minimax.<你的域名>
  service: http://localhost:8080
```

然后 `cloudflared tunnel route dns <tunnel> minimax.<你的域名>`（或直接在 CF 控制台加 CNAME 到 tunnel）。

**B. Nginx + CF A 记录**（橙云代理）：

```nginx
server {
    listen 80;
    server_name minimax.<你的域名>;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_buffering off;        # SSE 流式必需
        proxy_read_timeout 600s;
    }
}
```

TLS 按 VPS 上现有实例的惯例处理（CF 源站证书或 certbot）。CF 代理默认支持 SSE，无需额外设置。

> **视频模型不要进 New API**：`minimax-h3` / `minimax-hailuo-2-3` 一轮 15–30 分钟，同步等待会撞 CF 524。聊天/图像模型无此问题。

## 5. 管理台初始化

1. 浏览器打开 `https://minimax.<你的域名>`，粘贴 env 里的密钥登录（单密钥，无用户名）
2. **号池管理 → 添加账号**：粘贴 MiniMax 网页端 Local Storage 里的 `_token`（JWT）即可，区域自动识别、指纹自动生成、`realUserID` 服务端自动发现
3. **客户端密钥**：建一个 key（`sk-mm-…`）——这就是给 New API 的出口 Key

## 6. New API 渠道

| 项 | 值 |
| --- | --- |
| 类型 | OpenAI |
| Base URL | `https://minimax.<你的域名>` |
| 密钥 | 第 5 步的 `sk-mm-…` |
| 模型 | `minimax-agent`、`minimax-m3.1-flash-preview`、`minimax-m3`、`minimax-m3-thinking`、`minimax-m2.7`、`minimax-m2.7-highspeed`、`minimax-image` |

> `/v1/messages`（Anthropic 形状）同一域名同一 Key 也可用，供 Claude Code / Kiro 直连。

## 7. 收尾：密钥本地留底（必须）

把以下信息回填到本机 `F:\Minimax2api\secrets-local.md`：

- 管理台地址、初始管理员密码
- 客户端密钥 `sk-mm-…`
- New API 渠道名

## 日常运维

```bash
systemctl status minimax2api
journalctl -u minimax2api -f          # 日志
tar -C /opt/minimax2api -czf ~/minimax2api-data-$(date +%F).tar.gz data   # 备份（账号令牌都在 data/）
```

**升级**：本机重新 `Minimax2api.bat pack` → 上传覆盖二进制与 `dist/`（保留 `data/` 与 `env`）→ `systemctl restart minimax2api`。回滚：保留上一版二进制为 `minimax2api.bak` 即可换回。

> ⚠️ 管理台对公网开放（有登录 + HMAC 加盐存储）。若想更收紧，可在 CF 上对该子域套 Access 规则只放行管理路径，或 VPS 防火墙限制来源——按你现有实例的处理方式保持一致即可。
