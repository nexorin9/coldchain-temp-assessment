package ingest

import (
	"context"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
)

// 本文件是事件接入的落库入口：一条已规范化的温度事件进来，
// 先过同点限流，再幂等入库，最后把结果如实回给投递方。
//
// 顺序不是随意的：限流在前，是为了在洪泛时连库都不碰；
// 幂等在后，是因为「这条采样是不是已经收过」只有落库那一刻才知道。
//
// 入站这一段只回答「收到没有」，不回答「这段温度意味着什么」——
// 越温窗口、归因与成稿由判定链接手，事件接入不越界下结论。

// ingestStepName 是事件接入在服务日志里的步骤名。
//
// 入站这一段还没有 run_id：判定 run 是调度器按点位与越温窗口建的，
// 建在事件入库**之后**。带一个步骤名，是为了让「这一条采样进来了」
// 在按 run_id 串联之前也能被认出来——它属于整条判定链的最上游。
const ingestStepName = "ingest"

// Ingestor 是事件接入的落库入口。
type Ingestor struct {
	store   *store.Store
	limiter *RateLimiter
	logger  *logging.Logger
}

// NewIngestor 创建接入入口。limiter 为 nil 表示不限流；logger 为 nil 时回落到默认日志器。
func NewIngestor(st *store.Store, limiter *RateLimiter, logger *logging.Logger) *Ingestor {
	if logger == nil {
		logger = logging.Default()
	}
	return &Ingestor{store: st, limiter: limiter, logger: logger}
}

// IngestResult 是一次投递的结果。
type IngestResult struct {
	// EventID 是该采样在库里的标识。重复投递时与首次一致。
	EventID string
	// Duplicate 表示这次投递命中了已有的同一条采样，没有新写入。
	Duplicate bool
	// PointID 与 MeasuredAt 是这条采样的归属信息，回执里带回去，
	// 投递方不必自己再记一遍。
	PointID    string
	MeasuredAt time.Time
}

// Ingest 接入一条已规范化的温度事件。
//
// 三种结局：
//   - 正常入库：返回新事件 ID，Duplicate=false；
//   - 重复投递：返回既有事件 ID，Duplicate=true，库表里仍然只有一行；
//   - 触发限流：返回 E_RATE_LIMITED 错误，这次投递没有落库，
//     但拒绝这件事会被写进日志，不静默丢弃。
func (in *Ingestor) Ingest(ctx context.Context, ev domain.TemperatureEvent) (IngestResult, error) {
	if in == nil || in.store == nil {
		return IngestResult{}, errcode.Newf(errcode.StorageError, "事件存储未初始化")
	}

	if err := in.limiter.Allow(ev.PointID); err != nil {
		e := errcode.As(err)
		in.logger.Warn("ingest_rate_limited",
			"step_name", ingestStepName,
			"point_id", ev.PointID,
			"measured_at", ev.MeasuredAt.UTC().Format(time.RFC3339),
			"error_code", string(e.Code),
			"detail", e.Details)
		return IngestResult{}, err
	}

	outcome, err := in.store.InsertEvent(ctx, ev)
	if err != nil {
		e := errcode.As(err)
		in.logger.Error("ingest_store_failed",
			"step_name", ingestStepName,
			"point_id", ev.PointID,
			"measured_at", ev.MeasuredAt.UTC().Format(time.RFC3339),
			"error_code", string(e.Code),
			"detail", e.Details)
		return IngestResult{}, err
	}

	// 入站这一步还没有 run_id：判定 run 是调度器按点位与越温窗口建的，
	// 建在事件入库**之后**。日志里带 step_name 是为了让「这一条采样进来了」
	// 在按 run_id 串联之前也能被认出来——它属于整条判定链的最上游。
	if outcome.Duplicate {
		in.logger.Info("ingest_duplicate",
			"step_name", ingestStepName,
			"point_id", ev.PointID,
			"event_id", outcome.EventID,
			"measured_at", ev.MeasuredAt.UTC().Format(time.RFC3339))
	} else {
		in.logger.Info("ingest_stored",
			"step_name", ingestStepName,
			"point_id", ev.PointID,
			"event_id", outcome.EventID,
			"measured_at", ev.MeasuredAt.UTC().Format(time.RFC3339))
	}

	return IngestResult{
		EventID:    outcome.EventID,
		Duplicate:  outcome.Duplicate,
		PointID:    ev.PointID,
		MeasuredAt: ev.MeasuredAt,
	}, nil
}

// ImportSeries 批量导入一段记录仪导出的 CSV。
//
// 与逐条 Ingest 的差别有两条，都是有意为之：
//   - 不走同点限流闸门。限流挡的是设备端洪泛（网关重试风暴、时钟错乱导致的
//     集中补投）；批量导入是持凭据的人工操作，一次一个文件，由请求体上限兜底。
//     拿每分钟 60 条的限流去挡一次导三个月的历史数据，只会把正常作业挡在门外。
//   - 单事务落库。批处理跑到一半断电时，库里不会出现「前 300 条在、
//     后 200 条没进」的半截状态——半截的时序算出来的越温窗口是错的。
//
// 脏行不静默丢弃：跳过的每一行都带行号与原因回到调用方。
func (in *Ingestor) ImportSeries(ctx context.Context, data []byte, dryRun bool) (*ImportResult, error) {
	if in == nil || in.store == nil {
		return nil, errcode.Newf(errcode.StorageError, "事件存储未初始化")
	}

	result, err := ImportTemperatureSeries(ctx, in.store, data, dryRun)
	if err != nil {
		e := errcode.As(err)
		in.logger.Warn("import_failed",
			"step_name", ingestStepName,
			"error_code", string(e.Code),
			"detail", e.Details,
			"dry_run", dryRun)
		return nil, err
	}

	in.logger.Info("import_completed",
		"step_name", ingestStepName,
		"imported", result.Imported,
		"skipped", result.Skipped,
		"duplicates", result.Duplicates,
		"total_rows", result.TotalRows,
		"encoding", result.Encoding,
		"encoding_fallback", result.EncodingFallback,
		"original_bytes", result.OriginalBytes,
		"extra_columns", len(result.ExtraColumns),
		"extra_values", result.ExtraValues,
		"missing_columns", len(result.MissingColumns),
		"dry_run", dryRun)

	// 编码回退单独记一条 warn：这批数据已经不是设备当初写下的那个字节序列了，
	// 事后核对时需要知道这条，不能只混在导入完成日志里被翻过去。
	if result.EncodingFallback {
		in.logger.Warn("import_encoding_fallback",
			"step_name", ingestStepName,
			"encoding_from", "unknown",
			"encoding_to", result.Encoding,
			"original_bytes", result.OriginalBytes,
			"total_rows", result.TotalRows,
			"detail", "原始文件不是合法 UTF-8，已回退 GBK 解析；事件上已标 encoding_fallback")
	}
	return result, nil
}
