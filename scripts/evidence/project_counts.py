#!/usr/bin/env python3
"""把替身收到的真实报文投影成可读形式（台账写入体 + 门诊通知体 + 计数）。"""
import json
import os
import sys

raw = sys.stdin.read()
start = raw.find("{")
obj = json.loads(raw[start:]) if start >= 0 else {}

print("（这是冷链台账与通知系统两端真实收到的报文，按原样列出）")
print()

counts = obj.get("counts") or {}
print("出站调用计数：")
for key, value in counts.items():
    print(f"  {key}: {value}")
print()

def trim(body, key):
    """记录表正文很长，报文里留行数与前两行，正文另见 shot 04。"""
    out = dict(body or {})
    text = out.get(key)
    if isinstance(text, str) and len(text) > 400:
        lines = text.split("\n")
        out[key] = f"（记录表正文共 {len(lines)} 行，正文见 shot 04 的 coldchain draft 输出）\\n" + "\\n".join(lines[:2]) + "\\n…"
    return out


for item in obj.get("received") or []:
    kind = item.get("kind")
    if kind == "ledger_deviation_write":
        print("→ 冷链台账 POST /api/v1/cold-chain/deviations 收到的越温记录：")
        print(json.dumps(trim(item.get("body"), "exception_record"), ensure_ascii=False, indent=2))
        print()
    elif kind == "notify_message_send":
        print("→ 通知系统 POST /api/v1/messages 收到的门诊通知：")
        print(json.dumps(trim(item.get("body"), "body"), ensure_ascii=False, indent=2))
        print()
    elif kind == "ledger_batches_read":
        print(f"→ 冷链台账 GET {item.get('body', {}).get('path')}（读在库批次清单）")
    elif kind == "model_call":
        body = item.get("body") or {}
        print(f"→ 模型端点 POST /chat/completions（schema={body.get('schema')}, model={body.get('model')}）")
