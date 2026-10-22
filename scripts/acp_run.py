#!/usr/bin/env python3
"""跑一次 Hermes ACP 会话，把思考 / 回复 / 工具调用实时打到 stdout。

hermes -z 的设计是「只打印最终答案」，看不到推理过程；ACP 才有逐 token 的
agent_thought_chunk。CI 里用它替代 -z，日志就是完整的思考链。
"""
import json
import os
import subprocess
import sys
import threading


def text_of(content):
    """content 有时是 {type,text}，有时是 chunk 数组。"""
    if isinstance(content, dict):
        return content.get("text") or ""
    if isinstance(content, list):
        return "".join(c.get("text") or "" for c in content if isinstance(c, dict))
    return ""


def main():
    prompt = sys.argv[1]
    cwd = os.environ.get("ACP_CWD") or os.getcwd()
    p = subprocess.Popen(
        ["hermes", "acp", "--accept-hooks"], cwd=cwd,
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, bufsize=1)
    err = []

    def pump_stderr():
        # agent 起不来时原因全在 stderr；只留告警和错误，插件注册那类 INFO 太吵
        for line in p.stderr:
            if any(k in line for k in (" ERROR ", " WARNING ", "Traceback", "Error:")):
                err.append(line.rstrip())
                print("[acp] " + line.rstrip(), flush=True)

    threading.Thread(target=pump_stderr, daemon=True).start()

    def send(obj):
        try:
            p.stdin.write(json.dumps(obj) + "\n")
            p.stdin.flush()
        except BrokenPipeError:
            pass

    def read():
        line = p.stdout.readline()
        if not line:
            return None
        try:
            return json.loads(line)
        except json.JSONDecodeError:
            err.append(line.rstrip())
            return {}

    done = False
    send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
          "params": {"protocolVersion": 1, "clientCapabilities": {}}})
    while True:
        m = read()
        if m is None:
            print("\n[ACP 通道断开]", flush=True)
            break
        mid = m.get("id")
        method = m.get("method")
        if mid == 1:
            send({"jsonrpc": "2.0", "id": 2, "method": "session/new",
                  "params": {"cwd": cwd, "mcpServers": []}})
        elif mid == 2:
            sid = (m.get("result") or {}).get("sessionId")
            if not sid:
                print(f"[session/new 失败: {m.get('error')}]", flush=True)
                break
            # dont_ask：文件编辑不再弹审批（CI 里没人点，默认 ask 会被当拒绝）
            send({"jsonrpc": "2.0", "id": 4, "method": "session/set_mode",
                  "params": {"sessionId": sid, "modeId": "dont_ask"}})
        elif mid == 4:
            send({"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
                  "params": {"sessionId": sid,
                             "prompt": [{"type": "text", "text": prompt}]}})
        elif method == "session/update":
            u = m["params"]["update"]
            kind = u.get("sessionUpdate")
            text = text_of(u.get("content"))
            if kind == "agent_thought_chunk" and text:
                print(text, end="", flush=True)
            elif kind == "agent_message_chunk" and text:
                print(text, end="", flush=True)
            elif kind == "tool_call":
                title = u.get("title") or u.get("kind") or "tool"
                print(f"\n\n[tool] {title}", flush=True)
            elif kind == "tool_call_update" and u.get("status"):
                print(f"[tool {u.get('status')}]", flush=True)
        elif method == "session/request_permission":
            # yolo 下本该没有；真出现就一律允许。响应必须是
            # {"outcome":{"outcome":"selected","optionId":..}} —— 少这层包装
            # Hermes 会当成「被拒绝」，文件写入直接失败（真踩过）。
            opts = (m["params"].get("options") or [{}])
            picked = next((o for o in opts if "allow" in str(o.get("optionId", "")).lower()), opts[0])
            send({"jsonrpc": "2.0", "id": m["id"],
                  "result": {"outcome": {"outcome": "selected",
                                         "optionId": picked.get("optionId")}}})
        elif mid == 3:
            done = True
            print(f"\n\n[结束 stopReason={(m.get('result') or {}).get('stopReason')}]", flush=True)
            break
    try:
        p.stdin.close()
    except Exception:
        pass
    p.wait(timeout=30)
    if err:
        print("[acp stderr 末 5 行] " + " / ".join(err[-5:])[:400], flush=True)
    # 没收到 prompt 的 result 就算失败，别让上层当成「跑完了但没产出」
    return 0 if done else 1


if __name__ == "__main__":
    sys.exit(main())
