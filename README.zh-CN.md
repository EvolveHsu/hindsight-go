# Hindsight Go

[English](README.md) | [简体中文](README.zh-CN.md)

[Hindsight](https://github.com/vectorize-io/hindsight) Agent Memory API 的 Go 实现，基于上游 OpenAPI 3.1 契约生成。它提供独立的 Go REST API 和 MCP 服务，并可与官方 Hindsight Control Plane 前端配合使用。

## 功能特性

- **99 / 99 OpenAPI 操作全覆盖**：规范化后的上游契约中每一个操作都有生成 Handler 和 Go 实现。
- **REST 与 MCP 同端口**：`/v1/...` REST 路由和 `/mcp/{bank}/` Streamable HTTP MCP 共用同一套实现，工具调用与 API 调用行为一致。
- **完整的 Agent Memory 工作流**：retain、recall、reflect、consolidation、entities、temporal 和 semantic links、directives、bank aliases、operations、Knowledge Base、webhooks、transfer/import/export、文件保留。
- **PostgreSQL + pgvector 存储**：提供与上游 schema 兼容的 PostgreSQL 后端，支持生产部署和既有 Hindsight 数据迁移。
- **真实语义召回**：支持任意 OpenAI 兼容 embeddings 服务（TEI 可用），向量写入 pgvector，并与关键词召回通过 RRF 融合。
- **存储后端可插拔**：生产使用 PostgreSQL，测试或本地评估可使用进程内存储。
- **监控端点**：`/health`、`/health/live`、`/health/ready`、`/version` 和 Prometheus 风格 `/metrics`。

## 架构

Hindsight Go 聚焦 API 和 MCP 数据面。官方 [Hindsight Control Plane](https://github.com/vectorize-io/hindsight/tree/main/hindsight-control-plane) 可以作为独立前端服务运行，并指向这个 Go 后端。

```text
客户端 / Agent / MCP
        |
        +--> Hindsight Go :8890
              |-> REST /v1/...
              |-> MCP  /mcp/{bank}/
              |-> PostgreSQL + pgvector
              |-> OpenAI 兼容 LLM
              |-> OpenAI 兼容 embeddings
```

部署 compose 文件还会运行官方 Control Plane，服务名为 `hindsight-control-plane`。

## 快速开始

### 1. 最小运行

```bash
go run ./cmd/hindsight-go
```

默认使用进程内存储，监听 `http://0.0.0.0:8890`：

```bash
curl http://127.0.0.1:8890/health
```

### 2. 使用 PostgreSQL 和 embeddings

创建 `deploy/.env`：

```bash
HINDSIGHT_DB_PASSWORD=change-me
HINDSIGHT_GO_PORT=8890
HINDSIGHT_CP_PORT=9999
HINDSIGHT_MCP_BANK=default
HINDSIGHT_LLM_NETWORK=hindsight-go-llm

HINDSIGHT_GO_LLM_BASE_URL=https://api.example.com/v1
HINDSIGHT_GO_LLM_API_KEY=replace-with-api-key
HINDSIGHT_GO_LLM_MODEL=your-model

HINDSIGHT_GO_EMBEDDINGS_BASE_URL=http://hindsight-embedding:80/v1
HINDSIGHT_GO_EMBEDDINGS_MODEL=BAAI/bge-small-en-v1.5
HINDSIGHT_GO_EMBEDDINGS_DIMENSIONS=384
HINDSIGHT_GO_EMBEDDINGS_BATCH_SIZE=64
```

构建并启动完整栈：

```bash
docker compose -f deploy/docker-compose.yml --env-file deploy/.env up -d --build
```

服务地址：

- Hindsight Go API / MCP：`http://127.0.0.1:8890`
- 官方 Control Plane UI：`http://127.0.0.1:9999`
- PostgreSQL 和 TEI embeddings 只在 compose 内部网络访问。

生产部署建议把 PostgreSQL 和 embedding 服务保留在私有网络，只暴露确实需要的 Go API 和 Control Plane 端口。Go 容器还会加入 `HINDSIGHT_LLM_NETWORK`；如果你的 LLM 服务在其他 Docker 网络，请先创建该网络或修改这个变量。

## API 与 MCP

REST 路径遵循上游契约。常用操作包括：

```text
PUT    /v1/default/banks/{bank_id}
POST   /v1/default/banks/{bank_id}/memories
POST   /v1/default/banks/{bank_id}/memories/recall
GET    /v1/default/banks/{bank_id}/memories
GET    /v1/default/banks/{bank_id}/documents
POST   /v1/default/banks/{bank_id}/reflect
POST   /v1/default/banks/{bank_id}/consolidate
GET    /v1/default/banks
GET    /v1/default/banks/{bank_id}/tags
```

MCP 服务地址：

```text
http://127.0.0.1:8890/mcp/{bank}/
```

MCP 目录当前有 36 个工具，包括 `retain`、`sync_retain`、`recall`、`reflect`。MCP 调用会在进程内桥接到同一套 REST Handler，因此功能要么在两个入口同时可用，要么明确报告不支持。

## 数据与迁移

PostgreSQL 后端可以直接使用既有 Hindsight 上游 schema：

```bash
HINDSIGHT_GO_UPSTREAM_SCHEMA=1
```

这样既有 Python Hindsight 数据库可以交给 Go 后端接管，而无需改变数据模型。`deploy/migrate-from-python.sh` 提供了一次性割接流程：备份旧数据库、恢复到独立 PostgreSQL、启动 Go API 和 Control Plane。

在验证 Go 部署可用之前，不要删除旧数据目录或数据库备份。

## 开发

环境要求：

- Go 1.25 或更新版本
- Python 3.12，用于覆盖率检查脚本
- [ogen](https://github.com/ogen-go/ogen) v1.24.0，仅在需要重新生成 API 代码时使用

执行检查：

```bash
go build ./...
go vet ./...
go test ./... -count=1
python scripts/coverage_assert.py
```

Windows PowerShell 下：

```powershell
pwsh scripts/check.ps1
```

`internal/api` 中的生成代码有意提交到仓库，普通构建不需要重新生成。如果确实修改了 `openapi.go.json`，请重新生成并完整执行检查。

## 配置

### 生产必填

| 变量 | 说明 |
|---|---|
| `HINDSIGHT_GO_DATABASE_URL` | PostgreSQL DSN；不设置则使用进程内存储。 |
| `HINDSIGHT_DB_PASSWORD` | 仅 compose 使用的 PostgreSQL 密码。 |
| `HINDSIGHT_GO_LLM_BASE_URL` | OpenAI 兼容 LLM 服务地址。 |
| `HINDSIGHT_GO_LLM_API_KEY` | LLM API Key。 |
| `HINDSIGHT_GO_LLM_MODEL` | LLM 模型名称。 |

### Embeddings

| 变量 | 说明 |
|---|---|
| `HINDSIGHT_GO_EMBEDDINGS_BASE_URL` | OpenAI 兼容 embeddings 服务地址。 |
| `HINDSIGHT_GO_EMBEDDINGS_MODEL` | embeddings 模型名称。 |
| `HINDSIGHT_GO_EMBEDDINGS_DIMENSIONS` | 向量维度。 |
| `HINDSIGHT_GO_EMBEDDINGS_BATCH_SIZE` | embeddings 请求批大小。 |
| `HINDSIGHT_GO_EMBEDDINGS_API_KEY` | 可选 API Key。 |
| `HINDSIGHT_GO_EMBEDDINGS_QUERY_PREFIX` | 可选查询前缀。 |
| `HINDSIGHT_GO_EMBEDDINGS_DOCUMENT_PREFIX` | 可选文档前缀。 |

如果没有配置 embeddings 服务，recall 会明确回退到 token overlap，而不是假装执行了语义向量召回。

### 端口和路径

| 变量 | 默认值 | 说明 |
|---|---|---|
| `HINDSIGHT_GO_PORT` | `8890` | Go API / MCP 对外端口。 |
| `HINDSIGHT_CP_PORT` | `9999` | Control Plane 对外端口。 |
| `HINDSIGHT_MCP_BANK` | `default` | 割接验证使用的 bank 名称。 |
| `HINDSIGHT_LLM_NETWORK` | `hindsight-go-llm` | Go 容器用于访问外部 LLM 服务的 Docker 网络。 |
| `HINDSIGHT_GO_MCP_BANK_ID` | 未设置 | 裸 `/mcp` 端点使用的默认 bank。 |
| `HINDSIGHT_GO_MCP_INSTRUCTIONS` | 未设置 | MCP initialize 返回的 instructions。 |

### 运行时

| 变量 | 说明 |
|---|---|
| `HINDSIGHT_GO_UPSTREAM_SCHEMA` | 设置为 `1` 时使用上游兼容 PostgreSQL 表。 |
| `HINDSIGHT_GO_FILE_ROOT` | transfer 归档和文件存储根目录。 |

## 项目结构

```text
cmd/hindsight-go/       主程序
internal/api/           OpenAPI 生成 Handler 和类型
internal/server/        Engine、HTTP 路由、Store seam
internal/memory/        进程内存储后端
internal/storepg/       PostgreSQL + pgvector 后端
internal/model/         共享数据行、评分和融合逻辑
internal/mcp/           Streamable HTTP MCP 和 REST 桥接
internal/embeddings/    OpenAI 兼容 embeddings 客户端
internal/extract/       retain 阶段 LLM 事实抽取
internal/reflect/       reflect agent
internal/consolidate/   consolidation agent
deploy/                 生产 compose、迁移和验证脚本
scripts/                检查、smoke 测试和覆盖率断言
```

## 上游项目

这个项目用 Go 重新实现了 Hindsight Agent Memory API 的服务端，并依赖上游 API 契约。

- 上游仓库：[vectorize-io/hindsight](https://github.com/vectorize-io/hindsight)
- 上游 Control Plane：[vectorize-io/hindsight-control-plane](https://github.com/vectorize-io/hindsight/tree/main/hindsight-control-plane)
- 上游协议：MIT

Hindsight Go 是独立 Go 实现，不是 Vectorize 官方项目。

## 限制

与当前 Python 实现的已知有界差异：

- 文件保留对 Office 文档、OCR 和音频解析尚不完整。
- transfer 归档尚未完整包含附件字节。
- 后台 consolidation、webhook 投递和 mental-model cron refresh 还需要独立常驻 worker。
- 可选 recall reranking 以及部分 graph/temporal 召回策略仍然简化。

核心的 retain、recall、reflect、PostgreSQL 持久化、MCP 和公开 REST 接口已经实现并通过测试。

## 许可证

本项目采用 MIT License，详见 [LICENSE](LICENSE)。

仓库包含来自 Vectorize AI, Inc. 的 MIT 协议 Hindsight 项目衍生代码；上游版权声明已保留在 `LICENSE` 中。
