# `video-image` 二开维护记录

本文档是 `video-image` 分支的二开功能清单和上游同步检查表。后续合并官方 `upstream/main` 时，应把这里列出的“最终生效行为”视为需要保留或重新验证的兼容契约，不能只按提交信息机械保留代码。

## 1. 当前快照与差异基线

记录日期：2026-08-25。

| 项目 | 提交 |
| --- | --- |
| 二开分支 | `video-image` |
| 本文覆盖的最后一个业务二开提交 | `9a91facc` (`feat: expose server tool progress in chat streams`) |
| 最新上游合并提交 | `20473523` (`Merge branch 'main' into video-image`) |
| 已合入的官方基线 | `62d2775c` (`Merge pull request #1009 from chenyme/gateway`) |
| 记录时官方 `upstream/main` | `62d2775c` |

`video-image` 已于 2026-08-25 合入官方 `main` 的 `62d2775c`。当前二开差异可用以下命令复核：

```powershell
git diff --stat upstream/main...video-image
git diff --name-status upstream/main...video-image
git log --reverse --oneline main..video-image
```

三点语法会以当前已合入的官方基线为起点，只查看二开分支一侧仍然存在的最终差异。

## 2. 二开功能总览

| 模块 | 最终生效行为 | 主要文件 |
| --- | --- | --- |
| GHCR 构建 | `video-image` 推送触发镜像构建；当前只构建/合并 `linux/amd64` 镜像 | `.github/workflows/ghcr-image.yml` |
| Sora/new-api 视频兼容 | 新增 `POST /v1/videos`，兼容异步创建、查询和结果 URL | `inference/handler.go`、Swagger 文件 |
| Chat Completions 媒体模型 | Console 图片/视频模型可通过 `/v1/chat/completions` 使用；视频支持异步进度和最终播放器 | `console/chat_media.go`、`inference/handler.go` |
| 视频参考图 | 支持 `image`、`images`、`input_reference`、`reference_images` 及消息中的图片 URL | `handler.go`、`gateway/video.go`、`console/media.go` |
| 远程图片物化 | 上游无法下载参考图时，安全下载到本地并转为可提交输入；图片编辑复用同一策略 | `pkg/remotemedia/image.go`、`gateway/video.go`、`gateway/image.go` |
| OpenAI Images 兼容 | Generations/Edits 支持 JSON、multipart、多种图片字段、尺寸映射和兼容别名 | `handler.go`、Swagger、`console/catalog.go` |
| 固定 2K 图片别名 | 三个 `-2k` 模型别名在 Chat Completions、Images Generations、Images Edits 中都无条件将分辨率改为 `2k` | `domain/model/media_alias.go`、`gateway/image.go`、`console/chat_media.go` |
| multi-agent 默认工具 | 所有名称中含独立 `multi-agent` 段的 Console 模型默认补齐代码解释器、Web 搜索和 X 搜索 | `console/normalize.go` |
| 搜索/工具进度透传 | Responses 的服务端工具事件转换为 Chat Completions 的 `reasoning_content` 流 | `conversation/chat_server_tools.go`、`conversation/stream.go` |

## 3. 详细兼容行为

### 3.1 GHCR 镜像构建策略

- `.github/workflows/ghcr-image.yml` 的 push 分支包含 `video-image`。
- 二开工作流移除了 arm64 矩阵，只发布 amd64 镜像，并只用 `${tag}-amd64` 生成最终 manifest。
- 合并上游工作流时不能意外移除 `video-image` 触发器；如果恢复 arm64，应确认 runner 可用，并同步更新 README 中的架构说明。

### 3.2 Sora/new-api 风格视频接口

新增或增强以下路由：

```text
POST /v1/videos
POST /v1/videos/generations
GET  /v1/videos/{request_id}
GET  /v1/videos/{request_id}/content
```

`POST /v1/videos` 的兼容请求支持：

- `seconds` 或 `duration`，接受整数或整数字符串；默认 8 秒，有效范围 1–15 秒。
- `size`、`aspect_ratio`、`resolution`。
- 单图 `image`、`input_reference`。
- 多图 `images`、`reference_images`。
- 兼容任务 ID 使用 `video_sora_` 前缀；查询响应包含 `id`、`task_id`、`status`、`progress`、`seconds`、`size`，完成后在 `metadata.url` 返回结果地址。

`POST /v1/videos/generations` 继续使用官方风格响应，并扩展支持 `reference_images`。单图和多图列表合并后校验，总数不能超过领域层 `MaxInputImages` 限制。

视频结果 URL 优先指向本地归档资源 `/v1/media/videos/{asset_id}`；任务内容地址为 `/v1/videos/{request_id}/content`。内容接口支持 HTTP `Range`（底层对象可 seek 时），便于浏览器播放器拖动和分段读取。

主要冲突热点：

- `backend/internal/transport/http/inference/handler.go`
- `backend/internal/transport/http/swagger_annotations.go`
- `backend/docs/docs.go`、`swagger.json`、`swagger.yaml`
- `backend/internal/application/gateway/video.go`

### 3.3 Console 媒体模型走 Chat Completions

二开允许 Console 图片和视频模型通过 `POST /v1/chat/completions` 接入普通聊天客户端。

视频兼容模型识别包括 `grok-imagine-video`、`grok-imagine-video-1.5-console` 以及带 `Console/` 前缀的对应名称。Console 模型目录登记的是 `grok-imagine-video-1.5-console`，实际上游仍映射到 `grok-imagine-video`，用于避免与其他 Provider 的同名模型混淆。

请求兼容行为：

- 从最后一条有效 user message 提取提示词。
- 支持字符串 content 和内容数组中的文字、`image_url` 等参考图。
- 视频参数可在顶层或 `video_config` 中提供：`duration`、`seconds`、`size`、`aspect_ratio`、`resolution`。
- 可从提示词中的常见时长、横屏/竖屏/比例表达推断视频参数。
- 支持像素尺寸转宽高比和分辨率；视频比例限于 `1:1`、`16:9`、`9:16`、`4:3`、`3:4`、`3:2`、`2:3`。
- 多参考图视频最长限制为 10 秒。
- 图生视频未显式提供比例时保留当前 Console 回退策略；不要恢复后来已回滚的“强制自动比例”试验。

响应兼容行为：

- 视频任务异步入队，避免同步请求长时间占用上游创建阶段。
- `stream: true` 时输出任务状态/进度，任务完成后再输出结果，不能提前发送完成状态。
- 最终内容为 HTML5 `<video>` 播放器和 Markdown 下载链接，URL 指向本服务可访问的本地归档资源。
- 非流式请求在任务完成后返回标准 Chat Completions 结构。

主要冲突热点：`console/adapter.go`、`console/catalog.go`、`console/chat_media.go`、`console/media.go`、`inference/handler.go`。

### 3.4 视频参考图下载、物化与账号切换

远程图片下载能力被抽成 `backend/internal/pkg/remotemedia`，供视频输入和图片编辑共同使用。最终策略不是无条件下载所有 URL：

1. 优先把客户端提供的公开 URL 原样交给 Console 上游。
2. 只有上游明确返回图片下载失败时，服务端才下载并物化为本地/内联输入。
3. 物化后最多重试 3 次，并使用短暂退避。
4. Provider 支持托管出口时，通过对应账号/亲和出口下载；第三方图片请求不得携带 Provider token、cookie 或用户凭据。
5. 创建阶段只有明确的 402/429 配额拒绝才允许换账号；可能已创建异步任务的轮询错误不能盲目换号，防止重复任务。

远程图片下载的安全与兼容约束：

- 仅允许公开 `http`/`https` URL，端口限制为 80/443。
- 阻止 localhost、内网/保留地址、带用户信息的 URL 和 DNS rebinding。
- 每次重定向都重新解析并固定目标 IP，最多 5 次。
- 单图最大 20 MiB，仅接受 JPEG、PNG、GIF、WebP。
- 最多尝试 6 组浏览器/图片资源/Referer/Origin 请求头，兼容防盗链站点。
- 网络失败最多 3 次传输级尝试；401、403、406、408、425、429 和 5xx 可按策略重试。
- Base64 物化并发单独限制为 4，避免媒体并发放大内存峰值。
- 临时输入在任务成功或失败后释放。

图片编辑采用相同的“远程 URL 优先、上游下载失败后再物化”策略，识别 `image_download_error`、`image_download_interrupted` 和 `failed to download the provided image` 等诊断。

主要冲突热点：`pkg/remotemedia/image.go`、`gateway/video.go`、`gateway/image.go`、`application/media/service.go`、`infra/egress/manager.go`、`provider/provider.go`、`console/media.go`、`transport/http/media/ingest.go`。

### 3.5 OpenAI Images 兼容与固定 2K 别名

| 对外模型 | 实际 Console 上游 | 特殊行为 |
| --- | --- | --- |
| `grok-imagine-image-2k` | `grok-imagine-image` | 强制 `resolution=2k` |
| `grok-imagine-image-quality-2k` | `grok-imagine-image-quality` | 强制 `resolution=2k` |
| `grok-imagine-image-2.0-2k` | `grok-imagine-image-2.0` | 强制 `resolution=2k` |
| `gpt-image-1`、`gpt-image-1.5` | `grok-imagine-image-quality` | OpenAI 模型名兼容 |
| `dall-e-2`、`dall-e-3` | `grok-imagine-image` | OpenAI 模型名兼容 |

Images API 的最终兼容行为：

- `/v1/images/generations` 中出现 `image` 或 `images` 时自动转为图片编辑。
- `/v1/images/edits` 同时接受 JSON 和 multipart。
- multipart 识别 `image`、`image[]`、`images`、`images[]`；JSON 图片项可为 URL、data URL、file ID 或兼容对象。
- `size` 支持 `auto`、直接宽高比和任意正整数 `WIDTHxHEIGHT`，像素尺寸映射到最接近的 Grok 比例。
- 图片比例额外支持 `2:1`、`1:2`、`19.5:9`、`9:19.5`、`20:9`、`9:20`。
- `quality`、`background`、`output_compression`、`mask`、`user` 等 OpenAI 专用字段允许传入但不参与当前 Grok 请求。
- 固定 2K 行为在 Gateway 图片链路和 Console Chat Completions 媒体适配层应用，生成、编辑和聊天接口都生效，且不改变实际上游模型名。
- URL 输入被上游拒绝下载时，使用第 3.4 节的安全物化回退。

主要冲突热点：`domain/model/media_alias.go`、`gateway/image.go`、`console/catalog.go`、`inference/handler.go`、Swagger 注解及生成文件。

### 3.6 multi-agent 模型默认启用三类服务端工具

Console 请求归一化阶段检查“实际上游模型名”是否包含独立的 `multi-agent` 段，而不是硬编码具体版本。

- 匹配：`grok-4.20-multi-agent-0309`、`grok-4.5-multi-agent`、`grok-4.5-multi-agent-0401`。
- 不匹配：`multi-agentic` 等只是相似字符串的名称。

缺省补齐：

```json
{
  "tools": [
    {"type": "code_interpreter"},
    {"type": "web_search", "enable_image_understanding": true},
    {"type": "x_search", "enable_image_understanding": true}
  ],
  "tool_choice": "auto"
}
```

兼容规则：

- 保留客户端已有工具，只追加缺失项，不重复添加或覆盖显式设置。
- 显式 `tool_choice: "none"` 是关闭工具的逃生口，必须保留；`required` 仅在请求中保留了可由客户端执行的 function tool 时继续保留，否则会归一化为 `auto`。
- Web 搜索历史别名归一化为 `web_search`。
- `x_search` 使用 `enable_image_understanding`，不要误改回 `enable_video_understanding`。
- Responses、Chat Completions、Anthropic Messages 转入 Console Responses 请求后都应用同一规则。
- 默认提供三个工具不等于每次调用三个工具；`tool_choice: auto` 仍由模型决定。

主要冲突热点：`backend/internal/infra/provider/console/normalize.go` 及 `console_test.go`。

### 3.7 服务端工具进度转为 `reasoning_content`

上游 Responses 流中的 `web_search_call`、`x_search_call`、`code_interpreter_call` 会在 `/v1/chat/completions` 流中转换为 `choices[0].delta.reasoning_content`，例如：

```text
🔎 Web search: latest world news
✓ Web search completed
🔎 X search: breaking news
⚠ X search failed
🔎 Code interpreter started
✓ Code interpreter completed
```

实现约束：

- 这是通用协议转换，不依赖 RikkaHub、User-Agent、具体模型名或 `multi-agent` 判断。
- 服务端已经执行的 hosted tool 不能伪装为 OpenAI `tool_calls`，否则客户端会再次执行，可能循环或报错。
- 以 item ID 去重；无 ID 时按类型和输出序号生成键；最多跟踪 64 个调用。
- 查询文本合并空白并截断到 512 个 Unicode 字符。
- 优先处理 `response.output_item.added/done`；若上游只在最终 `response.completed.response.output` 返回工具项，则在完成阶段补发。
- Anthropic Messages 不泄漏 Chat Completions 专用的 `reasoning_content` 扩展。
- 上游不发送增量工具事件时，只能在响应完成附近看到补发信息，无法凭空实现实时进度。

主要冲突热点：`conversation/chat_server_tools.go`、`conversation/stream.go`、`conversation/conversation_test.go`。

## 4. 文件级二开范围

相对 `6e9eef76` 的业务二开（不含本文档自身）涉及 35 个既有或新增文件：

```text
.github/workflows/ghcr-image.yml
README.md
README.zh-CN.md
backend/docs/{docs.go,swagger.json,swagger.yaml}
backend/internal/app/console_routes_test.go
backend/internal/application/gateway/{image.go,image_test.go,service.go,service_test.go,video.go,video_test.go}
backend/internal/application/media/service.go
backend/internal/domain/model/{media_alias.go,media_alias_test.go}
backend/internal/infra/egress/{manager.go,manager_test.go}
backend/internal/infra/provider/console/{adapter.go,catalog.go,chat_media.go,console_test.go,media.go,normalize.go}
backend/internal/infra/provider/conversation/{chat_server_tools.go,conversation_test.go,stream.go}
backend/internal/infra/provider/provider.go
backend/internal/pkg/remotemedia/{image.go,image_test.go}
backend/internal/transport/http/inference/{handler.go,handler_test.go}
backend/internal/transport/http/media/{ingest.go,ingest_test.go}
backend/internal/transport/http/swagger_annotations.go
```

`backend/internal/transport/http/inference/handler.go` 是最大冲突热点，同时包含视频兼容、Chat 视频流、Images 兼容和公共媒体 URL。解决冲突时应分功能验证，不能对整个文件简单选择 ours/theirs。

## 5. 上游同步流程与防回归清单

```powershell
git fetch upstream
git switch video-image
git status --short --branch
git branch backup/video-image-before-sync-YYYYMMDD
git merge upstream/main
```

冲突解决后：

1. 对照第 3 节逐项检查最终行为，不只确认“能编译”。
2. 重新运行 `git diff upstream/main...video-image --stat`，确认二开差异仍在预期范围。
3. 如果上游已实现等价功能，应删除重复实现，但保留兼容契约和测试。
4. Swagger 注解变化后重新生成 `backend/docs`，不要只手改生成文件。`backend/internal/transport/http/swagger_annotations.go` 是唯一来源；CI 会执行 `make swagger` 并用 `git diff --exit-code` 校验三份生成文件。
5. 更新第 1 节基线/分支提交，并在第 7 节追加新二开提交。
6. 未经明确要求不要直接 push；先本地测试和复核差异。

重点防回归：

- `POST /v1/videos` 仍注册并受客户端密钥认证保护。
- `reference_images` 同时存在于解析和 Swagger 中。
- 视频完成结果使用本地可分享 URL，并支持播放器 Range 请求。
- Console Chat 视频不提前完成，流式结果包含进度和最终播放器。
- 多参考图视频最长仍为 10 秒。
- 远程图片下载保留 SSRF、重定向、大小、MIME、凭据隔离保护。
- Images Generations 带 `image/images` 时仍自动进入编辑流程。
- 三个 `-2k` 模型在三类兼容接口中仍强制 2K，四个 OpenAI 图片别名映射正确。
- 未来 `*-multi-agent-*` 模型仍补齐三个工具，`tool_choice: none` 仍能关闭。
- Chat 流把服务端工具进度放在 `reasoning_content`，而不是 `tool_calls`。
- GHCR 工作流仍为 `video-image` 构建预期架构镜像。

## 6. 回归测试

```powershell
cd backend
go test ./internal/infra/provider/conversation -count=1 -timeout=120s
go test ./internal/infra/provider/console ./internal/infra/provider/web ./internal/infra/provider ./internal/transport/http/inference ./internal/app -count=1 -timeout=150s
go test ./internal/application/gateway ./internal/pkg/remotemedia ./internal/infra/egress ./internal/transport/http/media -count=1 -timeout=150s
```

Windows 下完整测试套件曾出现 `internal/infra/egress` 的 SOCKS5H loopback 环境相关失败；没有新代码证据时不应直接归因于二开，应单独运行相关包区分逻辑问题与环境问题。

RikkaHub 手工验证请求：

```json
{
  "model": "grok-4.20-multi-agent-0309",
  "messages": [{"role": "user", "content": "搜索今天国内外重大新闻"}],
  "stream": true
}
```

预期：Console 上游请求包含三个默认工具和 `tool_choice: auto`；上游发送增量 hosted-tool 事件时，SSE 在最终文本前或附近出现 `choices[0].delta.reasoning_content`。

## 7. 二开提交索引

按时间顺序列出 `6e9eef76` 之后的提交。`1e50d30c` 是同步上游的合并提交；`78c1efaf` 已被 `46fb8f4e` 回滚，不代表当前行为。

| 提交 | 说明 |
| --- | --- |
| `6156d21a` | 增加 new-api/Sora 视频兼容 |
| `d18a5743` | 注册 Sora 视频创建路由 |
| `38d05840` | 覆盖 Sora 路由认证测试 |
| `5c8fe047` | 验证 Sora 路由进入认证链 |
| `10d0f8b2` | 恢复单数 Images 路由测试 |
| `0fda80e1` | 视频结果返回可分享本地 URL |
| `4fc331a4` | Console 媒体模型支持 Chat Completions |
| `73bd916f` | Console Chat 视频请求异步入队 |
| `8eba7bf8` | 稳定视频 Chat 任务消息格式 |
| `4b1a5be4` | 流式视频进度和公共结果地址 |
| `7f21377b` | Chat 视频结果输出 HTML 播放器 |
| `c4d4af28` | 从 Chat 请求推断视频比例和时长 |
| `3637a5e3` | 兼容视频秒数和像素尺寸/比例 |
| `6bd3eb3a` | 识别 Chat 提示词中的视频比例 |
| `3e6b5b3f` | 向 Console 视频上游转发参考图 |
| `ea833e7c` | 多参考图 Console 视频限制为 10 秒 |
| `2fbd7f88` | Console Imagine 视频模型命名空间隔离 |
| `0e805c33` | 延迟流式视频完成状态 |
| `3bacd8ff` | 保留图生视频比例并延迟流输出 |
| `be3c83e2` | 允许图生视频不显式指定比例 |
| `ef81ab4a` | 恢复 Console 图生视频比例回退 |
| `78c1efaf` | 尝试图生视频自动比例，后续已回滚 |
| `46fb8f4e` | 回滚 `78c1efaf` |
| `b55c99e0` | 视频接口适配 `reference_images` 并更新 Swagger |
| `1e50d30c` | 合并 `main` 到 `video-image`（非二开功能） |
| `e67b5406` | multi-agent 模型默认启用三类工具 |
| `212cf416` | 远程 URL 输入物化与下载逻辑抽取 |
| `a9b71f32` | 强化下载、托管出口和视频失败切换策略 |
| `ecae46f5` | 修正图片/视频尺寸到比例的映射 |
| `4ddac90b` | 增加固定 2K 图片模型别名 |
| `fd304e0b` | 完善 OpenAI Images Generations/Edits 兼容 |
| `90ddb0f5` | 强化图片生成/编辑 URL 参考图下载回退 |
| `9a91facc` | Chat 流展示服务端搜索和代码工具进度 |
| `20473523` | 合并最新 `main`（`62d2775c`）并保留二开媒体兼容 |

## 8. 维护原则

- 本文档记录“需要保留的行为”，提交哈希只是追溯线索。
- 上游有等价或更优实现时可以重构/删除二开代码，但应保留兼容性并更新测试和本文档。
- 新二开功能应同时补充功能说明、主要文件、冲突热点、回归测试和提交索引。
- 协议兼容应按能力或模型命名规律实现，避免硬编码临时版本；确需硬编码时须说明原因和替换条件。
- SSRF、凭据隔离、大小限制、异步任务去重等安全边界优先于表面兼容，合并冲突时不能为“请求能通”而删除保护。
