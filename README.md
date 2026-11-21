# loopforge-env

按分支构建并启动独立的 [tencent/LoopForge](https://github.com/tencent/LoopForge) 测试环境的 Web 管理平台。

每个环境是一套独立的 Compose 项目（platform + MySQL + 沙箱），拥有自己的端口、数据目录和镜像标签。构建通过队列执行，同一分支再次构建会更新到最新提交。

## 运行

需要 Docker（含 Compose 插件）、git、bash 和 Go 1.26。

```bash
go build -o lfenv-server .
LFENV_TOKEN='换成足够长的随机串' ./lfenv-server
```

打开 `http://<主机>:8088`，用 `LFENV_TOKEN` 登录。API 也接受 `Authorization: Bearer <token>`。

在服务器上以 systemd + Caddy（80/443、自动 HTTPS）部署的步骤见 [DEPLOY.md](DEPLOY.md)。

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LFENV_TOKEN` / `LFENV_TOKEN_FILE` | 必填 | 登录令牌；更换后旧会话全部失效 |
| `LFENV_BUILD_CONCURRENCY` | `2` | 同时构建的数量，其余排队 |
| `LFENV_ADDR` | `:8088` | 监听地址 |
| `LFENV_PUBLIC_HOST` | 请求的主机名 | 生成环境访问地址时使用的主机 |
| `LFENV_ENV_DOMAIN` | 空 | 设置后，`<环境名>.<域名>` 经平台端口反向代理到对应环境，例如 `env.example.com` 或 `203-0-113-10.sslip.io`；需要该域名的泛解析指向本机 |
| `LFENV_HOST` | 空 | 管理后台的公网主机名；前置代理按需签发证书时，只为它和已有环境放行（`GET /api/tls-allowed?domain=`） |
| `LFENV_POST_SCRIPT` | 空 | 环境启动后执行的初始化脚本，见下文 |
| `LFENV_POST_CONFIG` | 空 | 初始化脚本读取的配置文件；设置后可在页面上一起编辑，并传给脚本 |
| `LFENV_POST_TIMEOUT` | `600` | 初始化脚本的超时秒数 |
| `LFENV_PORT_BASE` | `18000` | 环境 HTTP 端口起始值 |
| `LFENV_ROOT` | `./.lfenv` | 代码、数据、任务记录和日志目录 |
| `LFENV_REPO` | `https://github.com/tencent/LoopForge` | 仓库地址 |
| `LFENV_GITHUB_TOKEN` | 空 | 读取 PR 标题、状态和列表用的 GitHub Token；不设时匿名访问（每小时 60 次，已用 ETag 缓存） |

## 按 PR 构建

输入框里填 `#478`、`PR 478`、`pull/478` 或 PR 链接即可按 PR 构建，下拉框也会列出打开的 PR，可按编号、标题、作者搜索。fork 提交的 PR 同样支持：构建的是 GitHub 在主仓库上维护的 `refs/pull/<编号>/head`，即 PR 作者推送的最新提交（不是与主干合并后的结果）。

PR 环境名为 `pull-<编号>`，访问地址如 `https://pull-478.<域名>`。重建会拉取 PR 的最新提交；PR 关闭或合并后环境不会自动销毁，列表里会显示 PR 状态。

PR 的代码以 root 权限构建运行，并能访问宿主机的 Docker socket，构建来自 fork 的 PR 前先看一眼改动。

## 按标签或提交构建

输入标签名（如 `v0.4.0`）或 7 到 40 位提交号（如 `a917e04`）即可；也接受 `tag:v0.4.0`、`commit:a917e04` 和 GitHub 的 commit、tree、release 链接。名字同时是分支和标签时按分支构建，十六进制串只有在不是分支或标签名时才当作提交。

提交号会展开成完整哈希：先查本地镜像（所有分支的历史），找不到再问 GitHub API，所以只存在于 PR 里的提交也能构建。环境名为 `tag-<标签>` 或 `commit-<前 12 位>`，列表里显示该提交的标题。重建标签会重新拉取标签（标签被移动时会更新），重建提交则是同一个提交。

## 一键登录

点环境的“打开”时，平台会尽量以初始化脚本配置里的管理员（`LFENV_POST_CONFIG` 的 `admin`）登录该环境：页面先申请一个 60 秒内有效、只能用一次的票据，新标签页打开 `https://<环境>.<域名>/__lfenv/login?t=…`，平台在本机用该账号登录 LoopForge，把登录 Cookie 设到环境域名上再跳到首页。没有配置管理员、没有设置 `LFENV_ENV_DOMAIN`、或密码已被改掉时，就只是打开首页。“访问地址”列的链接不带登录。

## 初始化脚本

设置 `LFENV_POST_SCRIPT` 后，每次环境启动成功（平台健康、数据库表补齐之后）都会在该分支的代码目录里执行这个脚本，用来创建账号、导入凭据等初始化数据。

自带的 [`post-init.py`](post-init.py) 只在环境首次启动时执行：创建管理员，保存管理员的模型 Key 和 GitHub Token，设置共享 CodeBuddy Key，创建项目并绑定仓库。密钥放在 `LFENV_POST_CONFIG`（默认 `/etc/lfenv/post-init.json`，格式见 [`post-init.example.json`](post-init.example.json)）。

脚本可以使用这些环境变量：

| 变量 | 说明 |
| --- | --- |
| `LFENV_BASE_URL` | 本机访问该环境的地址，如 `http://127.0.0.1:18000`；`AGENTFLOW_E2E_BASE` 与它相同 |
| `LFENV_FIRST_RUN` | 该环境的初始化脚本还没成功执行过时为 `1`，否则为 `0` |
| `LFENV_BRANCH` / `LFENV_SLUG` / `LFENV_COMMIT` | 分支、环境名、部署的提交 |
| `LFENV_PORT` / `LFENV_SRC` / `LFENV_COMPOSE_PROJECT` | 端口、代码目录、Compose 项目名 |

- 重建也会执行，脚本需要能重复运行。
- 输出写入构建日志，不要打印密钥。脚本失败或超时时构建任务标记为失败，环境本身仍在运行。
- 构建任务被取消时脚本一并停止。
- 脚本本身需要可执行；密钥建议放在脚本旁的 `600` 权限文件里，例如 `/etc/lfenv/`。

点环境列表上方的“首次启动执行 …”可以在页面上编辑脚本和 `LFENV_POST_CONFIG`。保存前会检查：配置必须是合法 JSON，脚本要有 `#!` 且能通过 Python / bash 语法检查；写入是原子替换，保留原有权限。两人同时编辑时，后保存的一方会被拒绝并提示重新加载。修改只影响之后首次启动的环境。

## 终端

环境的“终端”按钮在浏览器里打开容器的交互式 shell（优先 bash），可在 platform、mysql 和该环境的执行容器之间切换。执行容器是 LoopForge 为任务启动的 Docker 沙箱，按挂载了该环境数据目录来识别，进入后位于 `/workspace`。

终端经 WebSocket 连接（`/api/envs/<环境>/containers/<容器>/terminal`），平台通过 Docker socket 执行 `exec`；关闭页面或断开时会结束这个 shell。拥有登录令牌即拥有这些容器的 root shell。

## 命令行

`lfenv.sh` 是实际执行者，也可以单独使用：

```bash
./lfenv.sh up <branch>     # 构建并启动（再次执行即重建）
./lfenv.sh down <branch>   # 销毁容器、数据和代码目录
./lfenv.sh logs <branch>   # 查看 platform 日志
./lfenv.sh ls              # 列出环境
```

不要在平台运行时直接用命令行操作同一个环境；平台的队列不知道这些操作，会以“另一个操作进行中”失败。
