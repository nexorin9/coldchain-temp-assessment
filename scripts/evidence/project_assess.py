#!/usr/bin/env python3
"""把 assess 的真实 stdout 投影成可读结论（大载荷只留结论字段）。"""
import json
import os
import sys

raw = open(os.path.join(os.environ.get("COLDCHAIN_SHOT_WORK", "/tmp/coldchain-evidence"), "assess.stdout.json"), encoding="utf-8").read()
err = open(os.path.join(os.environ.get("COLDCHAIN_SHOT_WORK", "/tmp/coldchain-evidence"), "assess.stderr.txt"), encoding="utf-8").read()

start = raw.find("{")
obj = json.loads(raw[start:]) if start >= 0 else {}

print("（以下为 coldchain assess 的真实 stdout 结论字段；steps 的 payload 明细与 products 全量载荷见同目录 01-assess-run.full.json）")
print()

run = obj.get("run") or {}
print("run：")
for key in ["run_id", "point_id", "state", "exec_state", "window_start", "window_end",
            "external_ref", "idempotency_key", "zone", "degraded"]:
    if key in run:
        print(f"  {key}: {json.dumps(run[key], ensure_ascii=False)}")
print()

window = obj.get("window") or {}
print("window：")
for key, value in window.items():
    print(f"  {key}: {json.dumps(value, ensure_ascii=False)}")
print()

steps = obj.get("steps") or []
print("steps（判定步骤链，每步产物按分码独立存放）：")
print(f"  {'seq':<4}{'step_name':<20}{'state':<12}{'attempt':<9}{'duration_ms':<12}payload_kind")
for step in steps:
    print(f"  {step.get('seq',''):<4}{step.get('step_name',''):<20}{step.get('state',''):<12}"
          f"{step.get('attempt',''):<9}{step.get('duration_ms',''):<12}{step.get('payload_kind','')}")
print()

products = obj.get("products") or {}
attribution = products.get("attribution") or {}
if attribution:
    print("products.attribution（归因结论）：")
    for key in ["cause_code", "cause_label", "confidence", "degraded"]:
        if key in attribution:
            print(f"  {key}: {json.dumps(attribution[key], ensure_ascii=False)}")
    for item in attribution.get("evidence") or []:
        print(f"  evidence: {json.dumps(item, ensure_ascii=False)}")
    print()

record = products.get("exception_record") or {}
if record:
    print("products.exception_record（成稿记录表结构化载荷，摘要）：")
    for key in ["point_id", "device_name", "excursion_started_at", "excursion_ended_at",
                "peak_temperature_c", "duration_minutes", "zone", "cause_conclusion",
                "needs_review", "missing_fields"]:
        if key in record:
            value = record[key]
            if isinstance(value, list) and len(value) > 4:
                value = value[:4] + [f"…（共 {len(record[key])} 项）"]
            print(f"  {key}: {json.dumps(value, ensure_ascii=False)}")
    print()

confirmation = products.get("confirmation_ready") or {}
if confirmation:
    print("products.confirmation_ready（等待主管确认）：")
    for key, value in confirmation.items():
        print(f"  {key}: {json.dumps(value, ensure_ascii=False)}")
    print()

print("（stderr 摘要行）")
for line in err.strip().split("\n"):
    if line.strip():
        print("  " + line)
