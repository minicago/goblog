# Go 聚合静态博客服务器（支持 Git Hooks）

本项目是一个用 Go 编写的轻量级静态博客聚合服务器，具备以下能力：

- 从单个 Git 仓库的工作目录根目录读取 Markdown，文件名（去掉 `.md`）作为标题
- 使用 Markdown 生成静态页面（全站首页 + 各博客首页 + 单篇文章页）
- 通过 Git Hook 或 Git 托管平台 Webhook 触发自动重新构建
- 内置 HTTP 服务器直接托管生成后的静态文件

## 目录结构

```text
.
├── main.go             # HTTP 服务器入口
├── site/               # 静态站点生成逻辑
├── templates/          # HTML 模板（首页、文章页、help 等）
├── public/             # 生成后的静态网站（自动生成）
├── config.json         # 仓库配置（需自行创建）
└── go.mod
```

## 1. config.json：配置单个 Git 仓库（只需指定 url）

在项目根目录创建 `config.json`，示例如下（**仅一项**）：

```json
{
  "repos": [
    { "url": "git@github.com:you/tech-blog.git" }
  ]
}
```

程序会自动根据 URL 推导：

- 仓库名 `repoName`：取 URL 的最后一段并去掉 `.git`，如 `tech-blog.git` → `tech-blog`；
- `title`：若未提供，则默认使用 `repoName`，例如 `tech-blog`；
- `dir`：若未提供，则默认使用 `repos/repoName`，例如 `repos/tech-blog`。

当然，你也可以显式写出 `title` / `dir` 覆盖默认值，例如：

```json
{
  "repos": [
    { "url": "git@github.com:you/tech-blog.git", "title": "tech", "dir": "repos/tech" }
  ]
}
```

## 2. Git 仓库目录结构约定

当前版本只会扫描指定仓库的**根目录**下的 Markdown 文件，**不会递归子目录**。例如：

```text
project-root/
└── tech-blog/     # Git 仓库工作目录
    ├── post1.md
    ├── another.md
    └── subdir/    # 此目录中的 .md 文件不会被处理
```

每个 `.md` 文件的名称（去掉 `.md` 后缀）将作为文章标题，文件内容的第一行不再影响标题。

Markdown 语法支持表格、链接、图片、以及 LaTeX 公式 (KaTeX 渲染)。

> **新增**：仓库中的静态文件（例如 PNG/JPG 等图片、HTML 页面、CSS、图床资源等）
> 会被原样复制到 `public` 目录，支持相对路径引用。文章里使用的
> 相对链接如 `![...](foo.png)` 或 `[...](page.html)` 会被自动转为
> `/foo.png` `/page.html`，方便 Markdown 里直接写路径而不必顾虑
> 生成后页面所在目录。 这样即可把任何图片或者额外页面放在仓库中
> 并在文章中引用。

示例 `tech-blog/post1.md`：

```markdown
这是第一篇文章，文件名为 `post1`。

以下是一个表格：

| A | B |
|---|---|
| 1 | 2 |

行内公式 $E=mc^2$，块级公式：

$$
\int_0^1 x^2 dx = 1/3
$$
```

## 3. 启动服务器

在项目根目录执行：

```bash
go run ./...
```

默认：

- 监听地址：`http://localhost:8080`
- 配置文件：`config.json`
- 输出目录：`public`

可用的环境变量：

- `CONFIG_PATH`：配置文件路径（默认 `config.json`）
- `OUTPUT_DIR`：生成静态站点目录（默认 `public`）
- `LISTEN_ADDR`：HTTP 监听地址（默认 `:8080`）
- `WEBHOOK_PATH`：Webhook 路径（默认 `/webhook`）
- `WEBHOOK_TOKEN`：可选，用于简单鉴权（通过请求头 `X-Webhook-Token` 传递）

## 4. 访问路由说明

构建完成后，内置服务器通过 `http.FileServer` 托管 `public` 目录，对外暴露路由大致为：

- `/index`：全站最新文章列表（聚合所有仓库）
- `/`：与 `/index` 内容相同（首页）
- `/&lt;title&gt;`：对应某个博客的文章列表，例如 `/tech`、`/life`
- `/&lt;title&gt;/&lt;slug&gt;/`：具体某篇文章，例如 `/tech/post1/`
- `/help`：帮助页面（底部也提供了指向 `/help` 的链接）

> `slug` 默认为 Markdown 文件相对路径去掉 `.md` 后的结果，例如 `post1.md` ⇒ `post1`。

## 5. Git Hook / Webhook 集成

你可以对每个博客仓库单独配置 Git Hook，或者在托管平台（GitHub、Gitea 等）为这些仓库配置 Webhook，在内容变更时通知本服务重新构建。

### 5.1 方式 A：本地/裸仓库的 Git Hook

在你的 Git 仓库（通常是服务端裸仓库）的 `hooks/post-receive` 中添加脚本（Linux 示例）：

```bash
#!/bin/sh
curl -X POST "http://your-server:8080/webhook" \
  -H "X-Webhook-Token: your-token"
```

并确保服务器进程已设置：

```bash
export WEBHOOK_TOKEN=your-token
go run ./...
```

每次向该仓库 `git push` 后，Hook 会回调服务器，触发重新构建（会重新扫描所有配置的仓库目录）。

### 5.2 方式 B：GitHub / Gitea 等 Webhook

1. 在托管平台配置 Webhook，指向：
   - `http://your-server:8080/webhook`
2. 可在请求头自定义一个 `X-Webhook-Token: your-token`，并在服务器上设置同样的 `WEBHOOK_TOKEN`。
3. 收到 Push 事件时，服务器会重新调用生成逻辑。

> 若需要解析 GitHub/GitLab 的完整事件内容，可在 `main.go` 的 webhook 处理函数中读取 `r.Body` 并根据需要扩展逻辑（当前示例只关心“被调用”这件事，不解析具体 payload）。

