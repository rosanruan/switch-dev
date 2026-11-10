"""WorkBuddy 新旧端点对照验证脚本（一次性排查用，用完即删）。

从本机 info 文件读凭据，对照探测新旧域名/请求头组合：
  A. auth/state 旧域 + platform=workbuddy      （现状：已知可用）
  B. auth/state 新域 + platform=workbuddy-ai   （校验逻辑迁过去是否仍通）
  C. chat 旧域 + 现有代理头                    （复现线上 401）
  D. chat 新域 + 现有代理头                    （只换域名是否够）
  E. chat 新域 + 完整新头 + fast-model         （对齐新版 CLI）
  F. chat 新域 + 完整新头 + glm-5.0            （旧模型名是否仍可用，决定要不要改种子清单）
不打印任何 token 值。
"""
import base64
import http.client
import io
import json
import sys
import uuid
from urllib.parse import urlparse

sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8", errors="replace")

INFO = r"C:\Users\xyb\AppData\Local\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop-ai.info"

with open(INFO, encoding="utf-8") as f:
    d = json.load(f)
token = d["auth"]["accessToken"]
uid = d["account"]["uid"]
domain = d["auth"].get("domain", "")
print(f"auth.domain={domain!r} uid={uid[:8]}... token_len={len(token)}")

OLD = "https://copilot.tencent.com/v2"
NEW = f"https://{domain}/v2"

# 现有 switch-dev 代理发出的头（go: json.Marshal 的 X-Userinfo + Go 默认 UA）
userinfo_legacy = base64.b64encode(
    json.dumps({"sub": uid}, separators=(",", ":")).encode()
).decode()
LEGACY_CHAT_HEADERS = {
    "Accept": "application/json",
    "X-Userinfo": userinfo_legacy,
    "User-Agent": "Go-http-client/1.1",
}

# 新版 CLI 的会话头（个人账号：无 X-Userinfo、无企业头；fetch 删掉了 UA）
conv_id = uuid.uuid4().hex
msg_id = uuid.uuid4().hex
NEW_CHAT_HEADERS = {
    "Accept": "text/event-stream",
    "X-Product": "SaaS",
    "X-Domain": domain,
    "X-IDE-Type": "CLI",
    "X-IDE-Name": "CLI",
    "X-IDE-Version": "2.147.0",
    "X-Agent-Intent": "craft",
    "X-Agent-Type": "main",
    "X-Conversation-ID": conv_id,
    "X-Conversation-Request-ID": conv_id,
    "X-Conversation-Message-ID": msg_id,
    "X-Request-ID": msg_id,
}


def do_request(label, url, body, extra=None):
    p = urlparse(url)
    conn = http.client.HTTPSConnection(p.hostname, timeout=60)
    h = {
        "Host": p.hostname,
        "Content-Type": "application/json; charset=UTF-8",
        "Accept": "application/json",
        "Authorization": f"Bearer {token}",
        "X-Requested-With": "XMLHttpRequest",
        "X-User-Id": uid,
        "X-Private-Data": "false",
    }
    h.update(extra or {})
    try:
        conn.request("POST", p.path + ("?" + p.query if p.query else ""), body=body, headers=h)
        r = conn.getresponse()
        data = r.read(500)
        print(f"[{label}] HTTP {r.status}: {data[:260]!r}")
    except Exception as e:
        print(f"[{label}] ERR: {e}")
    finally:
        conn.close()


def chat_body(model):
    return json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 16,
        "stream": True,
    }).encode()


do_request("A auth OLD p=wb     ", OLD + "/plugin/auth/state?platform=workbuddy", b"")
do_request("B auth NEW p=wb-ai  ", NEW + "/plugin/auth/state?platform=workbuddy-ai", b"",
           {"X-Domain": domain, "X-Product": "SaaS"})
do_request("C chat OLD legacy   ", OLD + "/chat/completions", chat_body("fast-model"), LEGACY_CHAT_HEADERS)
do_request("D chat NEW legacy   ", NEW + "/chat/completions", chat_body("fast-model"), LEGACY_CHAT_HEADERS)
do_request("E chat NEW new fast ", NEW + "/chat/completions", chat_body("fast-model"), NEW_CHAT_HEADERS)
do_request("F chat NEW new glm-5", NEW + "/chat/completions", chat_body("glm-5.0"), NEW_CHAT_HEADERS)
