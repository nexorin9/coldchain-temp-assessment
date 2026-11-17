#!/usr/bin/env python3
"""把 GET /v1/runs/{id}/timeline 的真实响应投影成可读时间线。"""
import json
import os

obj = json.load(open(os.path.join(os.environ.get("COLDCHAIN_SHOT_WORK", "/tmp/coldchain-evidence"), "timeline.json"), encoding="utf-8"))

print("（真实响应原文字段；entries 不含 payload，这里按原样列出）")
print()

for key in ["run_id", "run_started_at", "run_ended_at", "total_duration_ms", "step_count"]:
    if key in obj:
        print(f"{key}: {json.dumps(obj[key], ensure_ascii=False)}")
print(f"current_step: {json.dumps(obj.get('current_step'), ensure_ascii=False)}")
print(f"recent_errors: {json.dumps(obj.get('recent_errors'), ensure_ascii=False)}")
print()

entries = obj.get("entries") or []
print("entries（每步的起止、耗时与在时间线上的位置）：")
print(f"  {'seq':<5}{'step_name':<20}{'state':<12}{'attempt':<9}{'duration_ms':<12}{'offset_ms':<12}{'start_pct':<11}width_pct")
for entry in entries:
    print(f"  {entry.get('seq',''):<5}{entry.get('step_name',''):<20}{entry.get('state',''):<12}"
          f"{entry.get('attempt',''):<9}{entry.get('duration_ms',''):<12}{entry.get('offset_ms',''):<12}"
          f"{entry.get('start_pct',''):<11}{entry.get('width_pct','')}")
