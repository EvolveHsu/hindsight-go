"""Record the completed v1.2.1 NAS sync through the production MCP endpoint."""
import json
import urllib.request


def rpc(tool, arguments):
    payload = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": tool, "arguments": arguments},
    }
    req = urllib.request.Request(
        "http://127.0.0.1:38462/mcp/codex/",
        data=json.dumps(payload).encode(),
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        },
    )
    with urllib.request.urlopen(req, timeout=420) as response:
        raw = response.read().decode()
    if raw.lstrip().startswith("{"):
        envelope = json.loads(raw)
    else:
        envelope = next(
            json.loads(line[5:])
            for line in raw.splitlines()
            if line.startswith("data:") and line[5:].strip()
        )
    assert "error" not in envelope, "MCP JSON-RPC error"
    result = envelope["result"]
    assert result.get("isError") is not True, "MCP tool failed"
    structured = result.get("structuredContent")
    if structured is None:
        structured = json.loads(result["content"][0]["text"])
    assert "error" not in structured, "MCP backend failed"
    return structured


document_id = "nas-hindsight-go-v1.2.1-sync-20261110"
content = (
    "2026-11-10：已将 NAS Hindsight Go 从上游 vectorize-io/hindsight v1.2.0 "
    "同步到 v1.2.1，Go服务版本0.1.4。上游 tag commit 0eb02d8c418eac6b489bc63883c993b2257dfac4。"
    "OpenAPI 操作从101增至109，新增8个 /pages 路由；旧 /knowledge-base 路由继续作为"
    "deprecated别名。同步了文档 add_tags/remove_tags、is_page、"
    "paused_mental_models、candidate_rejected状态、page_default_trigger和ID规则；"
    "MCP目录43工具，新旧Pages名称并存。Go服务容器 hindsight-go 端口38462，"
    "镜像 hindsight-go:v1.2.1-20261110；Control Plane容器 "
    "hindsight-control-plane端口38463，镜像1.2.1。测试通过全量单测、race、vet、"
    "109/109覆盖、Linux amd64静态构建、NAS候选及生产Pages/MCP验证。"
    "更新前备份 /vol1/1000/docker/hindsight-go-backup-v1.2.1-20261110，"
    "含项目归档、PostgreSQL dump、data-files归档、容器快照和SHA256SUMS；"
    "回滚容器 hindsight-go-rollback-v121-20261110（1.2.0），新源码目录 "
    "/vol1/1000/docker/hindsight-go/source-v1.2.1-20261110。"
    "既有数据库签名未变化。Compose更新时曾连带重建Go容器，已用部署快照恢复，"
    "最终配置按原始环境、挂载和网络核对一致。未清理旧镜像或构建缓存，清理需另行确认。"
    "仍为Go Lite实现，不宣称与官方Python worker、decision-model和扩展栈完全等价。"
)

result = rpc("sync_retain", {
    "document_id": document_id,
    "content": content,
    "tags": ["nas", "hindsight-go", "upgrade"],
    "context": "work",
})
assert result.get("success") is True and result["items_count"] > 0, "memory not retained"
recalled = rpc("recall", {
    "query": "hindsight-go v1.2.1 NAS update pages backup rollback",
    "tags": ["nas", "hindsight-go", "upgrade"],
    "tags_match": "all",
    "max_tokens": 8192,
})
assert any(
    item.get("document_id") == document_id
    for item in recalled.get("results", [])
), "new document not recalled"
print(json.dumps({
    "document_id": document_id,
    "retained": True,
    "facts": result["items_count"],
    "recalled": True,
}))
