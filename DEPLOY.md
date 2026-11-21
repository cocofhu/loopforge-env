# 部署

在一台 Linux 服务器上用 systemd 运行平台，Caddy 监听 80/443 并按需签发 HTTPS 证书。下文以 `203.0.113.10` 代表服务器公网 IP、`<令牌>` 代表登录令牌，按实际替换。

## 准备

- Linux x86_64，建议 4 核 8G 以上，磁盘按每个环境数 GB 预留（镜像、MySQL、代码）
- Docker 与 Compose 插件、git、bash、python3、curl
- [Caddy](https://caddyserver.com/docs/install) 2.x
- 安全组放行 TCP 80、443；平台端口 8088 和环境端口（默认 18000 起）不要对外开放

## 域名

每个环境通过子域名访问（`<环境名>.<域名>`），需要一个泛解析到本机的域名，二选一：

- 自有域名：添加 `*.env.example.com` 和 `env.example.com` 两条 A 记录指向服务器 IP
- 没有域名：用 [sslip.io](https://sslip.io)，`203-0-113-10.sslip.io` 及其任意子域名都解析到 `203.0.113.10`，无需配置

下文以 `203-0-113-10.sslip.io` 为例。

## 构建与安装

在本地交叉编译（前端已嵌入二进制）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o lfenv-server .
scp lfenv-server lfenv.sh post-init.py post-init.example.json <服务器>:/tmp/
```

在服务器上：

```bash
sudo install -d -m 0755 /opt/lfenv
sudo install -m 0755 /tmp/lfenv-server /tmp/lfenv.sh /tmp/post-init.py /opt/lfenv/
sudo install -m 0644 /tmp/post-init.example.json /opt/lfenv/

# 登录令牌与初始化配置只放在 /etc/lfenv，权限 600
sudo install -d -m 0700 /etc/lfenv
openssl rand -hex 24 | sudo tee /etc/lfenv/token >/dev/null
sudo chmod 600 /etc/lfenv/token
sudo install -m 0600 /opt/lfenv/post-init.example.json /etc/lfenv/post-init.json
sudo vi /etc/lfenv/post-init.json   # 填入管理员账号、模型 Key、GitHub Token、仓库等
```

`sudo cat /etc/lfenv/token` 即为页面登录令牌。

## systemd

`/etc/systemd/system/lfenv.service`：

```ini
[Unit]
Description=LoopForge branch environment manager
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
WorkingDirectory=/opt/lfenv
Environment=HOME=/root
Environment=LFENV_ADDR=127.0.0.1:8088
Environment=LFENV_TOKEN_FILE=/etc/lfenv/token
Environment=LFENV_PUBLIC_HOST=203.0.113.10
Environment=LFENV_ENV_DOMAIN=203-0-113-10.sslip.io
Environment=LFENV_HOST=203-0-113-10.sslip.io
Environment=LFENV_BUILD_CONCURRENCY=2
Environment=LFENV_POST_SCRIPT=/opt/lfenv/post-init.py
Environment=LFENV_POST_CONFIG=/etc/lfenv/post-init.json
# 可选：读取 PR 信息时避免匿名限流
# Environment=LFENV_GITHUB_TOKEN=...
ExecStart=/opt/lfenv/lfenv-server
Restart=on-failure
RestartSec=3
KillMode=mixed
TimeoutStopSec=40

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now lfenv
systemctl status lfenv
```

平台只监听 `127.0.0.1:8088`，由 Caddy 对外。环境数据默认在 `/opt/lfenv/.lfenv`（`LFENV_ROOT` 可改）。`LFENV_GITHUB_TOKEN` 也可以放进 `/etc/lfenv/` 下的文件，用 `EnvironmentFile=` 引入，避免写在 unit 里。

## Caddy

`/etc/caddy/Caddyfile`：

```caddyfile
{
	on_demand_tls {
		ask http://127.0.0.1:8088/api/tls-allowed
	}
}

# 直接用 IP 访问时跳到 HTTPS 域名
http://203.0.113.10 {
	redir https://203-0-113-10.sslip.io{uri} 308
}

https:// {
	tls {
		on_demand
	}
	reverse_proxy 127.0.0.1:8088 {
		flush_interval -1
	}
}
```

```bash
sudo systemctl reload caddy
```

- 证书在首次访问某个主机名时签发；`ask` 只为 `LFENV_HOST` 和已存在的环境放行，防止被人刷证书
- `flush_interval -1` 让构建日志和终端（WebSocket）实时传输

打开 `https://203-0-113-10.sslip.io`，用令牌登录。

## 更新

```bash
# 本地
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o lfenv-server .
scp lfenv-server <服务器>:/tmp/

# 服务器：确认没有进行中的构建（重启会中断它们）
curl -s -H "Authorization: Bearer $(sudo cat /etc/lfenv/token)" 127.0.0.1:8088/api/state \
  | python3 -c 'import json,sys; q=json.load(sys.stdin)["queue"]; print(q["building"], q["queued"])'
sudo install -m 0755 /tmp/lfenv-server /opt/lfenv/ && rm /tmp/lfenv-server
sudo systemctl restart lfenv
```

`lfenv.sh`、`post-init.py` 有改动时同样复制到 `/opt/lfenv/`。重启平台不影响已运行的环境。

## 安全

- 令牌持有者可以构建任意代码、拿到环境容器的 root shell，并能经 Docker socket 控制宿主机，请当作服务器 root 密码对待；更换 `/etc/lfenv/token` 后重启服务，旧会话全部失效
- `/etc/lfenv/post-init.json` 含密钥，保持 `600`，不要提交到仓库
- 环境内的 LoopForge 默认对公网可访问，管理员密码不要用弱口令
