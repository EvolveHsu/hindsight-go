# hindsight-go 工作约定

通用流程以上级为准：

- `/Users/shaw/Documents/ChatGPT/nas/AGENTS.md`
- `/Users/shaw/Documents/ChatGPT/nas/docs/NAS-应用更新SOP.md`

## 触发方式

用户说「按 NAS 更新 SOP 处理 hindsight-go」时，同步上游 API/Control Plane
契约并更新 NAS 部署。

## 项目事实

- 上游：`https://github.com/vectorize-io/hindsight`
- 官方 Control Plane：
  `https://github.com/vectorize-io/hindsight/tree/main/hindsight-control-plane`
- 当前 Go 服务版本：`0.1.4`
- 当前上游 API 契约：`1.2.1`
- NAS 数据面容器：`hindsight-go`
- NAS API/MCP 端口：`38462 -> 8890`
- Control Plane：`hindsight-control-plane`，`38463 -> 9999`
- PostgreSQL：`hindsight-postgres`，pgvector `pg18`
- Embedding：`hindsight-embedding`

## 更新要求

1. 对比上游 OpenAPI、Control Plane 版本和数据库 schema。
2. 文档先行，随后更新生成代码、Lite 数据面实现和回归测试。
3. 运行项目已有全量测试、race、vet 和接口覆盖检查。
4. 部署前必须：
   - 备份 NAS 项目目录；
   - 对 `hindsight-postgres` 执行 `pg_dump`；
   - 记录 banks/documents/memory_units 等关键表行数；
   - 确认 pgvector extension 和 schema。
5. 保留 `hindsight-go`、Control Plane、PostgreSQL、Embedding 的网络和卷。
6. 部署后验证 REST `/health`、MCP initialize、真实 retain/recall、Control Plane
   连通性和数据库行数。
7. 不修改全局模型、LLM 网络或 reflect 推理配置，除非用户明确要求。

## 数据红线

- 不输出数据库密码、连接串、Token、API Key 或完整记忆内容。
- 不直接修改用户长期记忆，除非任务明确要求并先备份数据库。
- 旧镜像/构建缓存仅在用户确认后清理。

