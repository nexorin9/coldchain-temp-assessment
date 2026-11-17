#!/usr/bin/env python3
"""把 GET /v1/runs/{id} 的真实响应投影成可读结论。"""
import json
import os

obj = json.load(open(os.path.join(os.environ.get("COLDCHAIN_SHOT_WORK", "/tmp/coldchain-evidence"), "run.json"), encoding="utf-8"))

print("（真实响应原文字段；steps 的 payload 明细已省略，完整响应见同目录 02-run-timeline.txt 的原始采集）")
print()

run = obj.get("run") or {}
print("run：")
for key, value in run.items():
    print(f"  {key}: {json.dumps(value, ensure_ascii=False)}")
print()

window = obj.get("window") or {}
print("window：")
for key, value in window.items():
    print(f"  {key}: {json.dumps(value, ensure_ascii=False)}")
print()

steps = obj.get("steps") or []
print("steps：")
for step in steps:
    print(f"  seq={step.get('seq')} {step.get('step_name')} state={step.get('state')} "
          f"attempt={step.get('attempt')} duration_ms={step.get('duration_ms')} "
          f"payload_kind={step.get('payload_kind')}")
print()

print(f"step_count: {obj.get('step_count')}")
print(f"current_step: {json.dumps(obj.get('current_step'), ensure_ascii=False)}")
print(f"recent_errors: {json.dumps(obj.get('recent_errors'), ensure_ascii=False)}")
print()

products = obj.get("products") or {}
attribution = products.get("attribution") or {}
if attribution:
    print("products.attribution：")
    for key in ["cause_code", "cause_label", "confidence", "degraded", "degrade_reason"]:
        if key in attribution:
            print(f"  {key}: {json.dumps(attribution[key], ensure_ascii=False)}")
    for item in attribution.get("evidence") or []:
        print(f"  evidence: {json.dumps(item, ensure_ascii=False)}")
