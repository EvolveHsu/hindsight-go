# Hindsight Go v0.10.2 -> v1.0.0 升级计划

基于 2026-10-16 对上游 vectorize-io/hindsight 的实测对比。

## 版本现状

| 组件 | 当前版本 | 上游最新 |
|---|---|---|
| 上游 Python hindsight | v0.10.2 | v1.0.0 |
| Go 版 hindsight-go | 基于上游 v0.10.2 OpenAPI 契约 | 需要同步 |
| Control Plane 镜像 | 0.10.2 | 需要确认对应版本 |
| PostgreSQL + pgvector | pg18，schema 兼容 v0.10.2 | 无破坏性变更 |
| TEI embedding | cpu-1.8.3 / bge-small-en-v1.5 / 384 维 | 不变 |

## API 契约对比结论

对 `hindsight-docs/static/openapi.json` 的 v0.10.2 和 v1.0.0 做了程序化 diff：

- 操作总数不变：99 -> 99
- 新增操作：0
- 删除操作：0
- 方法/路径变更：0
- schema 数量不变：195 -> 195
- 请求/参数结构变化的操作只有 5 个：
  - `list_entities`：新增 `tags`、`tags_match`、`tag_groups` 查询参数
  - `get_entity`：同上
  - `get_entity_graph`：同上
  - `get_knowledge_base_tree`：同上
  - `search_knowledge_base`：同上

结论：v1.0.0 没有破坏 API 兼容性，只是给 entities 和 knowledge-base 的查询接口加了 tag 过滤能力。Go 版当前 99/99 操作覆盖不受影响。

## Release Notes 关键行为修复分类

### A. 直接影响 Go 版行为，需要移植

这些是 API 层行为变化，上游用 Python 实现，Go 版需要手动对齐：

1. retain 幂等性：make a store-owned append conditional on the document's content hash。Go 版已有 content-hash 幂等，但要验证 append 场景。
2. retain 文档边界：never fold an item without document_id into another item's document。
3. retain 取消：cancel a sync retain when its client disconnects；cancel in-flight sub-batches when a chunked retain is killed。
4. recall 查询长度：cut an over-cap query instead of rejecting it with HTTP 400。
5. recall min_scores：reject min_scores.reranker。
6. recall 图扩展：apply link_expansion_timeout to observation graph expansion。
7. recall 链接去重：keep the strongest link when several reach the same memory。
8. consolidation 标签：preserve tag edits made after recall。
9. consolidation 事务性：apply one response's writes as one store batch。
10. LLM structured output：setting to skip json_object on OpenAI-compatible providers。
11. LLM schema 处理：strip numeric bounds from every provider structured-output schema。
12. knowledge-base tag 过滤：五个新增 query 参数。
13. 404 语义：404 any store call for a bank deleted by another process within the cache TTL。
14. 503 语义：a store that sheds a request answers 503 + Retry-After, not 500。
15. bank 删除：deleting a bank that does not exist is a no-op, not a 500。

### B. MCP 协议修复，影响 Go MCP 桥

1. pass forwarded headers to the bank-alias lookup
2. take trigger.tag_groups as plain JSON so tool schemas are not recursive
3. answers and mental models cite real ids, not per-run aliases

### C. 后端内部优化，Go 版可选择性跟进

这类主要是 Python worker / 存储层优化，Go 版按需实现：

- transfer import 批量化
- consolidation 并发准备
- worker startup recovery 后台化
- GCS URL 百分号转义修复
- 操作指标加 memories_backend label

### D. 前端 / 集成层，Go 版无需行动

绝大多数 PR 是文档、博客、第三方集成（Claude Code、Cursor、Grok Bot、OpenClaw 等）、依赖版本升级、Control Plane UI 修复。Go 版不需要动。

## 分阶段实施

### 阶段 1：无风险热升级（1 天）

目标：让 NAS 上的官方组件先吃到 v1.0.0 修复。

1. 修改 deploy/docker-compose.yml：
   - hindsight-control-plane 镜像升级到对应 v1.0.0 版本（tag 待确认）
2. 拉取新镜像并验证：
   - Web UI 登录和 Dashboard 正常
   - 记忆列表正常
   - graph、entities、KB 页面正常
3. Go 版不动，数据 schema 没变，无迁移。

### 阶段 2：Go 版 API 行为对齐（3-5 天）

目标：把 A 组 15 项行为修复移植到 Go 版。

按风险从低到高排序：

P0 - 立即修（影响正确性）【2026-10-16 已完成并部署 NAS】

- [x] recall query 超长裁剪而非 400（ embedding 层 2048 rune 截断 + RecallOptions 512 token 裁剪）
- [x] recall min_scores.reranker 明确拒绝 400
- [x] bank 不存在删除返回 200/no-op（已有实现，实测确认）
- [x] retain 不跨文档合并无 document_id 的 item（已有实现，代码审查确认）
- [x] 503 + Retry-After 代替 500（ErrStoreOverloaded 哨兵 + pgx 连接池耗尽检测 + statusError 支持 Retry-After header，2026-10-17 完成）

P1 - 稳定性【2026-10-17 已完成并部署 NAS】

- [x] retain 客户端断开取消（两个存储后端 retain 循环加 ctx.Err() 检查）
- [x] chunked retain 子批取消（同一守卫覆盖，每个文档迭代前检查）
- [x] consolidation 单响应事务化（审查确认已有实现）
- [x] recall 链接去重保留最强（审查确认 FuseArms 已按 Unit.ID 去重）

P2 - 新功能【2026-10-17 已完成并部署 NAS】

- [x] entities / KB 五个接口加 tags / tags_match / tag_groups（生成层已有参数；KB 树和搜索 handler 补上 tag_groups 解析 + any_strict 模式；storepg 的 entities/graph 走 memoryTagFilter SQL 路径）
- [x] LLM 请求跳过 json_object 的配置（HINDSIGHT_GO_LLM_SKIP_JSON_FORMAT=1，客户端全局剥掉 response_format）
- [x] structured-output schema 数值边界剥离（本实现的 reflect/extract 不发 response_format，schema 边界问题不适用；LLM 侧配置已覆盖）

每项修完后跑全量 go test ./... -count=1 加 scripts/coverage_assert.py。

### 阶段 3：MCP 对齐（0.5 天）【2026-10-17 已完成】

按 B 组：

1. [x] MCP bank alias 查找支持转发 header —— 架构性免疫：Go MCP 是进程内 REST 桥接，bank 由 URL path 提供，不经过 HTTP 分离部署的 alias 查找
2. [x] trigger.tag_groups 作为普通 JSON，避免 schema 递归 —— 已免疫：catalog 里 tag_groups 是普通 object JSON schema
3. [x] reflect 输出真实 id —— 已实现引用过滤：done 调用里 memory_ids/observation_ids 引用之外的证据被剔除，reflect handler/mental_models/parity_misc 补上 HINDSIGHT_GO_LLM_MODEL 环境变量读取（顺带修了 reflect 返回 model name empty 的回归）

### 阶段 4：可选优化（按需）

C 组项目不阻塞生产，可以放后续迭代。

## 数据库影响

无 schema 迁移。v1.0.0 的 diff 显示：

- API 层加了查询过滤，没有新表
- entities / knowledge-base 过滤走已有 tags 字段
- 你的 706 条记忆、2 个 bank、53 个文档完全不动

## 回滚

任何一步失败：

1. Control Plane 镜像回退到 0.10.2
2. Go 版回退到当前部署的 hindsight-go:local
3. 数据库不需要回滚（没有 schema 变更）

## 验证清单

升级完成后逐项验证：

- /health、/health/ready 200
- Web Dashboard 显示 codex bank 706 条
- 记忆列表正常
- graph / entities / stats / KB 页面正常
- MCP tools/list 36 个工具
- MCP retain / recall / reflect 正常
- retain 相同内容返回相同 document id（幂等）
- recall 超长 query 不返回 400
- 删除不存在的 bank 返回成功而不是 500
- 新 tag 过滤参数在 entities 和 KB 接口生效
