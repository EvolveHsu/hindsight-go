"""Restore requested operational memories through the production MCP endpoint."""
import json
import urllib.request


def rpc(tool, arguments):
    payload = {
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": tool, "arguments": arguments},
    }
    req = urllib.request.Request(
        "http://127.0.0.1:38462/mcp/codex/",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json",
                 "Accept": "application/json, text/event-stream"},
    )
    with urllib.request.urlopen(req, timeout=420) as response:
        raw = response.read().decode()
    if raw.lstrip().startswith("{"):
        envelope = json.loads(raw)
    else:
        envelope = next(json.loads(line[5:]) for line in raw.splitlines()
                        if line.startswith("data:") and line[5:].strip())
    assert "error" not in envelope, "MCP JSON-RPC error"
    result = envelope["result"]
    assert result.get("isError") is not True, "MCP tool failed"
    structured = result.get("structuredContent")
    if structured is None:
        structured = json.loads(result["content"][0]["text"])
    assert "error" not in structured, "MCP backend failed"
    return structured


memories = [
    {
        "document_id": "nas-muse2api-go-upstream-sync-20261107",
        "tags": ["nas", "muse2api-go"],
        "query": "muse2api-go 1.6.9 deployment upstream",
        "content": (
            "2026-11-07：已将 Python czg86389-hub/muse2api v1.6.7至v1.6.9增量吸收到"
            "EvolveHsu/muse2api-go，并部署 NAS 192.168.3.180:38467，镜像 muse2api-go:1.6.9。"
            "上游HEAD/tag v1.6.9均为8a3dab9db13b23ac694d21af29e65504bc7e69b1，"
            "Go基线14e4592是1.6.6。本次包括混合素材、新版输入框/CDP上传、原素材过滤、"
            "代理测试及管理页，保留Go版既有生命周期差异，不宣称全量上游等价。"
            "3个账号凭据配置和Docker设置保留，仅保活checked_at更新。"
            "全量单测/race、CI vet、72组Python提示词对照、桌面手机Playwright通过；"
            "未做真实计费生成。当前源码/vol1/1000/docker/muse2api-go/ctx，"
            "旧源码ctx-pre-1.6.9-20261107；回滚容器muse2api-go-rollback-1.6.6-20261107；"
            "备份/vol1/1000/docker/muse2api-go/backup-pre-1.6.9-20261107。未commit/push。"
        ),
    },
    {
        "document_id": "nas-hindsight-retain-fix-20261107",
        "tags": ["nas", "hindsight-go"],
        "query": "hindsight-go retain truncation fix token budget",
        "content": (
            "2026-11-07修复NAS Hindsight Go写入500：deepseek-v4.1-flash推理占用固定2048"
            "输出预算，JSON被截断；旧代码未检查finish_reason=length。"
            "现默认抽取预算8192、上限32768，最多3次调用，截断时翻倍重试；"
            "配置变量HINDSIGHT_GO_RETAIN_MAX_TOKENS和HINDSIGHT_GO_RETAIN_MAX_TOKENS_LIMIT。"
            "完整JSON校验、取消处理和脱敏用量日志已加入，不保存截断事实。"
            "全量单测/race/vet和101接口覆盖检查通过，NAS隔离候选已真实验证模型重试、"
            "MCP写入和召回，正式端口仍38462。当前镜像hindsight-go:retain-fix-20261107，"
            "回滚容器hindsight-go-rollback-20261107，备份"
            "/vol1/1000/docker/hindsight-go/backup-retain-fix-20261107；"
            "新源码/vol1/1000/docker/hindsight-go/source-retain-fix-20261107。"
            "未改数据库结构、全局模型或reflect推理；之前审计的其他Lite实现差异未在本次修复。"
            "NAS根目录旧源码仅保留历史，不是新构建源。"
        ),
    },
]

for memory in memories:
    result = rpc("sync_retain", {
        "document_id": memory["document_id"],
        "content": memory["content"], "tags": memory["tags"], "context": "work",
    })
    assert result.get("success") is True and result["items_count"] > 0, "memory not retained"
    recalled = rpc("recall", {
        "query": memory["query"], "tags": memory["tags"], "tags_match": "all",
        "max_tokens": 8192,
    })
    assert any(item.get("document_id") == memory["document_id"]
               for item in recalled.get("results", [])), "new document not recalled"
    print(json.dumps({"document_id": memory["document_id"], "retained": True,
                      "facts": result["items_count"], "recalled": True}), flush=True)
