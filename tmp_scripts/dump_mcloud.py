import json
from mitmproxy import http

def response(flow: http.HTTPFlow):
    if "assistant/chat/v2/add" not in flow.request.url:
        return

    try:
        body = json.loads(flow.request.content)
    except Exception:
        return

    cont = body.get("continuationInfo")
    dialogue = body.get("dialogueInput", {}).get("dialogue", "")[:200]

    print(f"\n{'='*60}")
    print(f"[REQUEST] {flow.request.url}")
    print(f"continuationInfo: {json.dumps(cont, ensure_ascii=False)}")
    print(f"dialogue (first 200 chars): {dialogue}")

    # Extract sessionId/dialogueId from SSE response frames
    resp_text = flow.response.text or ""
    for line in resp_text.split("\n"):
        if line.startswith("data:") and "sessionId" in line:
            data_str = line[5:].strip()
            try:
                data = json.loads(data_str)
                sid = data.get("data", {}).get("sessionId")
                did = data.get("data", {}).get("dialogueId")
                if sid:
                    print(f"[RESPONSE] sessionId={sid}, dialogueId={did}")
                    break
            except json.JSONDecodeError:
                pass
