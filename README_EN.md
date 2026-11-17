# Drug & Vaccine Cold-Chain Temperature Deviation Assessment System

> The recorder pushes in a temperature event; within minutes the pharmacy cold-chain administrator gets an **excursion window + the batches in stock at that moment + a cause assessment + a ready-to-submit draft of the "Temperature Abnormal Condition Record Form"** — and only after the supervisor confirms does the cold-chain ledger entry and the vaccination-clinic notification get written.

## What this is, and who it is for

An HTTP API service (Go, standard-library `net/http`) that takes over the work that follows a cold-chain temperature excursion: figuring out which unit went out of range, from when, how long, and how far; pulling the batches that were in that unit at that moment; reaching a documented cause; and producing the form the pharmacy committee and the vaccine manufacturer actually ask for. Vaccine cold-storage freezers, vaccination-clinic refrigerators and pharmacy drug fridges record at 5-minute granularity; this service consumes those readings and turns the post-incident investigation into one auditable, repeatable workflow.

It serves four roles: the pharmacy cold-chain administrator (operator — triggers assessments, reads conclusions), the pharmacy supervisor (the only identity that may confirm or reject a disposition), the vaccination-clinic nurse (receives an isolation notice), and QC / infection control (read-only reviewer for monthly internal audits). **Whether a batch may still be used is never decided by this service** — the vaccine manufacturer assesses the quality impact, and the pharmacy supervisor confirms the disposition.

## Core capabilities

- **Excursion windows form automatically, and dirty data is never counted as an excursion.** Consecutive out-of-range samples are extracted into one window per point temperature zone; data gaps, clock regressions and stuck probes are each recognised and labelled on the window; where the zone came from is recorded in the conclusion.
- **Cause assessment carries evidence, and every number the model reports must be findable in the on-site readings.** The peak temperature must exist in the recorder readings (±0.05 °C), the window bounds must fall inside the rule-extracted window, and the batch numbers must appear in the ledger's in-stock list. Anything that does not add up is sent back to the model once; if it still does not add up, the conclusion falls back to the rule baseline and the blocked field is archived.
- **In-stock batches are pulled once, and each batch gets its own usability advice.** Batch numbers come only from the ledger — the service never invents one. When one fridge holds both 2–8 °C vaccines and 0–10 °C refrigerated drugs, a 9 °C peak is an excursion for the former and not the latter, so each batch is advised against its own zone, exposure time and deviation.
- **The abnormal-condition record form is drafted directly.** Every column traces back to a source field; anything unavailable at that moment is written as "to be filled" rather than a plausible-looking value; insufficient sensor accuracy and rule-baseline fallback are printed on the first line of the draft instead of buried at the end.
- **Nothing is written back before confirmation; after confirmation it is written once, and can be compensated.** Model and rule output always stops at "awaiting pharmacy supervisor confirmation". Once confirmed, the payload is frozen and sent unchanged: first the cold-chain ledger, then the clinic notification. If the ledger write fails, the excursion is queued for compensation replay with the same idempotency key — the ledger never gains a duplicate row.
- **Each role sees only its own slice, and every step can be replayed.** One token per identity behind a least-privilege role matrix; each assessment is reconstructable as a step timeline; model calls and human confirmations share one audit chain; a 24-hour operating summary is available read-only; and historical readings can be replayed offline before a flight inspection and compared field by field with the original conclusion.

## Quick start

This is an **HTTP API service with no graphical interface** — use `curl` against the endpoints, or `api/openapi.yaml` for the full contract.

```bash
# 1. Requirements: Go >= 1.22 (pure-Go SQLite driver, no cgo needed)
git clone <this repository> coldchain-temp-assessment
cd coldchain-temp-assessment
go mod download
go build -o bin/coldchain ./cmd/coldchain

# 2. Configure: every base URL and credential is read from the environment only
cp .env.example .env        # fill in your hospital's real values; .env is git-ignored
set -a && . ./.env && set +a

# 3. Run the service (HTTP API on :8080)
./bin/coldchain serve
curl -s http://127.0.0.1:8080/healthz

# 4. One-shot verification: env check -> build -> tests -> start service -> push one event
#    -> print the assessment conclusion and the step timeline
bash scripts/verify.sh
```

Key configuration (all environment variables; required unless noted): `COLDCHAIN_INGEST_TOKEN` (recorder / edge-gateway identity, may only push events), `COLDCHAIN_OPERATOR_TOKEN` (pharmacy cold-chain administrator), `COLDCHAIN_SUPERVISOR_TOKEN` (pharmacy supervisor — the only identity that may confirm), `COLDCHAIN_AUDITOR_TOKEN` (QC / infection control, optional), `COLDCHAIN_LEDGER_BASE_URL` + `COLDCHAIN_LEDGER_TOKEN` (cold-chain ledger, read and write), `COLDCHAIN_NOTIFY_BASE_URL` + `COLDCHAIN_NOTIFY_TOKEN` (notification system), `MODEL_BASE_URL` + `MODEL_API_KEY` + `MODEL_NAME` (OpenAI-compatible model endpoint), `DATABASE_PATH` (local SQLite file). A missing required entry makes `coldchain serve` exit immediately with code 78 — there is no localhost fallback.

Common commands: `coldchain assess` (assess one event or a series), `coldchain draft` (print the record form of one assessment), `coldchain batches` (in-stock batches for a point at a given moment), `coldchain export` (weekly or hourly CSV), `coldchain archive` (archive aged raw samples), `coldchain replay` (offline replay and field-by-field comparison), `coldchain bench` (offline benchmark on public cold-chain datasets), `coldchain version`.

## Repository layout

```
coldchain-temp-assessment/
├── cmd/coldchain/   # CLI entry point: serve / assess / import / replay / draft / batches / archive / export / bench / version
├── internal/        # Core logic: config, ingest, ledger, rules, runtime, workflow, model, store, report, drafting, observe, httpapi, contract, bench, audit, fixture
├── api/             # OpenAPI 3.1 contract for every endpoint
├── testdata/        # De-identified samples: readings, point profiles, recorder exports, in-stock batches, dirty-data shapes
├── scripts/         # Dataset fetch, offline benchmark, contract check, one-command verification, evidence capture
├── docs/            # Benchmark and dataset notes, concepts/, screenshots/
├── data/ logs/      # Runtime data (created at run time, git-ignored)
├── .env.example     # Configuration template: placeholders only, no real credentials
└── go.mod README.md LICENSE
```

Full documentation: see README.md (Chinese)

---

## Follow Us

Scan the QR code to follow our official account for updates and community access:

![Follow Us](qrcode.jpg)