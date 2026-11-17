#!/usr/bin/env python3
"""冷链判定服务的出站测试替身（本次只用于采集实测截图）。

三个角色：
  * 冷链台账（ledger）  —— 读点位时序、读在库批次、写越温偏差
  * 通知系统（notify）  —— 发接种门诊通知
  * 模型端点（model）   —— OpenAI-compatible /chat/completions

它不是产品的一部分：产品代码里没有任何 localhost 默认值，
这些替身只是在本机占住冷链台账 / 通知系统 / 模型端点的位置，
好让真实服务跑出一条完整的判定 → 确认 → 写回链路。
"""
import json
import os
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LEDGER_PORT = int(os.environ.get("COLDCHAIN_STUB_LEDGER_PORT", "18141"))
NOTIFY_PORT = int(os.environ.get("COLDCHAIN_STUB_NOTIFY_PORT", "18142"))
MODEL_PORT = int(os.environ.get("COLDCHAIN_STUB_MODEL_PORT", "18143"))

# 样例目录按脚本自身位置推导（scripts/evidence/ → 项目根 → testdata/），
# 不写死任何人的本机绝对路径——换个机器 clone 下来照样能跑。
PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
TESTDATA = os.path.join(PROJECT_ROOT, "testdata")

COUNT_LOCK = threading.Lock()
COUNTS = {
    "ledger_series": 0,
    "ledger_batches": 0,
    "ledger_deviations": 0,
    "notify_messages": 0,
    "model_calls": 0,
}
RECEIVED = []


def load_series():
    with open(f"{TESTDATA}/series_excursion.json", encoding="utf-8") as fh:
        raw = json.load(fh)
    out = []
    for item in raw:
        out.append({
            "measured_at": item["measured_at"],
            "temperature_c": item["temperature_c"],
            "humidity_pct": item.get("humidity_pct"),
            "accuracy_c": item.get("accuracy_c"),
        })
    return out


def load_batches():
    out = []
    with open(f"{TESTDATA}/batches_sample.csv", encoding="utf-8") as fh:
        header = fh.readline().rstrip("\n").split(",")
        for line in fh:
            line = line.rstrip("\n")
            if not line.strip():
                continue
            cells = line.split(",")
            row = dict(zip(header, cells))
            out.append({
                "batch_ref": row["batch_ref"],
                "point_id": row["point_id"],
                "product_name": row["product_name"],
                "quantity": int(row["quantity"]),
                "expiry_date": row["expiry_date"],
            })
    return out


def bump(key):
    with COUNT_LOCK:
        COUNTS[key] += 1


def record(kind, body):
    with COUNT_LOCK:
        RECEIVED.append({"kind": kind, "at": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "body": body})


class Base(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def _send(self, code, payload):
        raw = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _body(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0:
            return {}
        raw = self.rfile.read(length)
        try:
            return json.loads(raw.decode("utf-8"))
        except Exception:
            return {"_raw": raw.decode("utf-8", "replace")}


class LedgerHandler(Base):
    def do_GET(self):
        if self.path.startswith("/api/v1/points/") and self.path.endswith("/series"):
            bump("ledger_series")
            record("ledger_series_read", {"path": self.path})
            self._send(200, {"samples": load_series()})
            return
        if self.path.startswith("/api/v1/batches"):
            bump("ledger_batches")
            record("ledger_batches_read", {"path": self.path})
            self._send(200, {"batches": load_batches()})
            return
        if self.path == "/__counts":
            with COUNT_LOCK:
                self._send(200, {"counts": dict(COUNTS), "received": RECEIVED})
            return
        self._send(404, {"error": "not_found", "path": self.path})

    def do_POST(self):
        body = self._body()
        if self.path == "/api/v1/cold-chain/deviations":
            bump("ledger_deviations")
            record("ledger_deviation_write", body)
            self._send(200, {"ok": True, "accepted_ref": body.get("external_ref")})
            return
        self._send(404, {"error": "not_found", "path": self.path})


class NotifyHandler(Base):
    def do_POST(self):
        body = self._body()
        if self.path == "/api/v1/messages":
            bump("notify_messages")
            record("notify_message_send", body)
            self._send(200, {"ok": True, "message_id": "msg-stub-0001"})
            return
        self._send(404, {"error": "not_found", "path": self.path})

    def do_GET(self):
        if self.path == "/__counts":
            with COUNT_LOCK:
                self._send(200, {"counts": dict(COUNTS), "received": RECEIVED})
            return
        self._send(404, {"error": "not_found", "path": self.path})


CAUSE_LABEL = {
    "power_loss": "断电",
    "door_open": "开门",
    "defrost": "除霜",
    "probe_detached": "探头脱落",
    "probe_stuck": "探头卡死",
    "ac_failure": "空调故障",
    "overstock": "超量存放",
    "unknown": "原因待定",
}


def build_attribution(payload):
    """按送进来的采样与窗口算一条归因结论。

    数值全部来自载荷本身（事件条数、峰值），这样结论里的数字
    与事实校验依据同源，不会被判「事实不符」而退回规则基线。
    """
    samples = payload.get("samples") or []
    window = payload.get("window") or {}
    counts = {"door_open": 0, "power_loss": 0, "defrost": 0}
    for sample in samples:
        for event in sample.get("events") or []:
            if event in counts:
                counts[event] += 1

    order = ["power_loss", "door_open", "defrost"]
    cause = next((code for code in order if counts[code] > 0), "unknown")
    confidence = 0.86 if cause != "unknown" else 0.3

    evidence = []
    for code in order:
        if counts[code] > 0:
            evidence.append({
                "feature": f"device_event:{code}",
                "value": float(counts[code]),
                "threshold": 1.0,
                "note": f"窗口内上报 {CAUSE_LABEL[code]} 事件的采样条数",
            })
    if window.get("peak_c") is not None:
        evidence.append({
            "feature": "peak_temperature_c",
            "value": float(window["peak_c"]),
            "threshold": 8.0,
            "note": "窗口内峰值温度",
        })

    candidates = [{
        "cause_code": cause,
        "label": CAUSE_LABEL[cause],
        "confidence": confidence,
        "evidence": evidence,
        "note": "证据来自记录仪上报的设备事件与窗口峰值，需药学部主管复核",
    }]
    return {
        "cause_code": cause,
        "label": CAUSE_LABEL[cause],
        "confidence": confidence,
        "candidates": candidates,
        "evidence": evidence,
        "notes": ["结论按记录仪上报的设备事件与越限窗口给出，处置仍需药学部主管确认"],
    }


class ModelHandler(Base):
    def do_POST(self):
        body = self._body()
        bump("model_calls")
        schema_name = ""
        try:
            schema_name = (body.get("response_format") or {}).get("json_schema", {}).get("name", "")
        except Exception:
            schema_name = ""

        content = None
        if schema_name == "attribution":
            payload = {}
            for message in reversed(body.get("messages") or []):
                if message.get("role") == "user":
                    try:
                        payload = json.loads(message.get("content") or "{}")
                    except Exception:
                        payload = {}
                    break
            content = json.dumps(build_attribution(payload), ensure_ascii=False)
        elif schema_name == "batch_advice":
            content = "[]"
        elif schema_name == "disposition_note":
            content = json.dumps({
                "disposition": "isolate",
                "note": "建议隔离待评估",
                "rationale": "峰值超过温区上限",
                "confidence": 0.6,
            }, ensure_ascii=False)
        else:
            content = "{}"

        record("model_call", {"schema": schema_name, "model": body.get("model")})
        self._send(200, {
            "id": "chatcmpl-stub-0001",
            "object": "chat.completion",
            "model": body.get("model") or "stub-model",
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": content},
                "finish_reason": "stop",
            }],
            "usage": {"prompt_tokens": 512, "completion_tokens": 128, "total_tokens": 640},
        })


def serve(port, handler):
    server = ThreadingHTTPServer(("127.0.0.1", port), handler)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


if __name__ == "__main__":
    serve(LEDGER_PORT, LedgerHandler)
    serve(NOTIFY_PORT, NotifyHandler)
    serve(MODEL_PORT, ModelHandler)
    print(f"stub up ledger={LEDGER_PORT} notify={NOTIFY_PORT} model={MODEL_PORT}", flush=True)
    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        sys.exit(0)
