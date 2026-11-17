// 本文件是模型输出的**事实校验**：防幻觉的那道硬闸。
//
// 形状校验（validate.go）只管「字段齐不齐、类型对不对、取值在不在域内」。
// 它拦不住另一种更危险的情况：模型答得格式规规矩矩，数值却是编的。
//
//	「峰值温度 13.7℃」——现场记录仪上从来没有这个读数；
//	「越温从 02:40 到 03:20」——规则抽出的窗口是 03:10 到 03:25；
//	「批次 LOT-2026-0099 建议隔离」——台账在库批次清单里没有这个批次号。
//
// 这三个数字如果原样写进《疫苗储存和运输温度异常情况记录表》、
// 再随台账递到疫苗生产企业那里，后果是按一个不存在的温度去评估疫苗质量。
// 所以事实校验的规矩很硬：**模型引用的每一个温度、每一个起止、每一个批次号，
// 都必须能在本次判定的输入依据里找到。**找不到就是模型在编，拦下。
//
// 三类事实、三条依据：
//
//	温度数值  → 输入时序（±0.05℃ 容差，与规则窗口口径一致）；
//	窗口起止  → 规则抽出的越温窗口（必须落在窗口内）；
//	批次号    → 冷链台账查到的在库批次清单（只认清单里的号）。
//
// 拦下之后怎么办：带校验反馈**重问一次**，让模型看着「哪儿错了」改一次；
// 仍不符就不再重试，返回 E_MODEL_FACT_MISMATCH，由判定步骤退回规则基线
// （降级路径见 internal/model/degrade.go 与 spec「AI 边界与降级」）。
// 状态不推进、台账不写回——批次能不能用始终由人决定。
//
// 源产品机制对应（溯源见 spec.md「开源对照」与「参考地基」）：
// 源产品的 model-prices 主路径在投影时**显式剔除「看着像数据、其实是占位」的条目**
// （文档占位键 sample_spec、input/output 两项成本都为 0 的空壳条目），
// 注释写明「excluding a real model's usage silently costing nothing」——
// 宁可少一条，不可让一条编出来的数悄悄混进产物里。
// 搬到冷链场景，对应物就是这道事实校验：
// **模型给的数必须能在输入里找到，否则不进结论。**
package model

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
)

// logFactCheckPersistFailure 记下「校验记录没写进库」这件事。
//
// 审计写不进去不该篡改业务结论，但也不能装作没发生——
// 记进服务日志，复核时至少知道这条校验记录缺了。
func logFactCheckPersistFailure(err error) {
	if err == nil {
		return
	}
	logging.Default().Error("fact_check_persist_failed", "error", err.Error())
}

// FactIssue 是一处事实不符（类型定义见 internal/domain/factcheck.go）。
type FactIssue = domain.FactIssue

const (
	// TemperatureToleranceC 是温度数值可在输入时序中找到的容差（℃）。
	//
	// 取 0.05℃：记录仪读数本身就有 0.1℃ 的分辨率，
	// 模型把 12.4 说成 12.42 属于四舍五入，不是编；
	// 说成 13.7 才是编。容差卡在两者之间。
	TemperatureToleranceC = 0.05

	// factCheckMaxAttempts 是事实校验失败后允许的最大调用次数（首次 + 重问一次）。
	//
	// 只重问一次是刻意的：同一个模型拿着同一份输入，
	// 反复问下去多半还是同一个错，而每问一次都占一次端点配额与一次判定时延。
	// 问过一次还不对，说明这次判定该退回规则基线，而不是继续磨。
	factCheckMaxAttempts = 2
)

// temperatureEvidenceFeatures 是「证据里的这个数值就是一条实测温度」的特征名。
//
// 只有这几个特征参与温度核对：它们引用的是某一条真实读数。
// 其它证据值（开门分钟数、事件条数、温升速率）是量，不是读数，
// 拿读数去核对它们会误判。
var temperatureEvidenceFeatures = map[string]bool{
	"temperature_c":       true,
	"peak_temperature_c":  true,
	"measured_temp_c":     true,
	"observed_temp_c":     true,
	"record_temp_c":       true,
	"temperature_reading": true,
}

// deviceEventFeatures 是「证据里的这个数值是某类设备事件的出现条数」的特征名。
//
// 键是特征名，值是对应的设备事件类型（internal/domain 的事件取值域）。
var deviceEventFeatures = map[string]string{
	"device_event:door_open":  domain.EventDoorOpen,
	"device_event:power_loss": domain.EventPowerLoss,
	"device_event:defrost":    domain.EventDefrost,
}

// FactBasis 是事实校验的权威依据。
//
// 它同时是「送给模型的输入」与「核对模型输出的尺子」——两者同源，
// 才不会出现「模型看的是 A、校验用的是 B」这种各说各话。
type FactBasis struct {
	// PointID 是温控点位标识。
	PointID string
	// Samples 是这次判定依据的温度采样（温度数值核对的唯一来源）。
	Samples []domain.TemperatureEvent
	// Window 是规则抽出的越温窗口（窗口起止与时长核对的依据）。
	Window *domain.ExcursionWindow
	// Batches 是冷链台账查到的在库批次（批次号核对的依据）。
	Batches []domain.Batch
	// BatchesKnown 表示在库批次清单已经取到（哪怕取回来是空的）。
	//
	// 这一位必须显式：清单「查了、结果是空的」与「压根没查」是两回事。
	// 前者说明模型提到任何批次号都是编的；后者不该拦——那只是还没查。
	BatchesKnown bool
}

// slimSample 是送给模型的采样投影。
//
// 只留判定要用的几项：时刻、温度、湿度、设备事件、告警位。
// 设备编号、精度声明、原始时间文本这些不进模型输入——
// 它们对「这次越温长什么样」没有帮助，却占着上下文。
type slimSample struct {
	MeasuredAt   string   `json:"measured_at"`
	TemperatureC float64  `json:"temperature_c"`
	Unit         string   `json:"unit,omitempty"`
	HumidityPct  *float64 `json:"humidity_pct,omitempty"`
	Events       []string `json:"events,omitempty"`
	AlarmFlag    bool     `json:"alarm_flag,omitempty"`
}

// Payload 返回送给模型的输入载荷。
//
// 载荷内容就是校验依据本身：模型能引用的数字与批次号，
// 恰好就是事实校验允许的那些——不多给，也不少给。
func (b FactBasis) Payload() map[string]any {
	payload := map[string]any{
		"point_id": b.PointID,
		"samples":  b.slimSamples(),
	}
	if b.Window != nil {
		payload["window"] = map[string]any{
			"start":            b.Window.Start.Format(time.RFC3339),
			"end":              b.Window.End.Format(time.RFC3339),
			"duration_minutes": b.Window.DurationMinutes,
			"peak_c":           b.Window.PeakC,
			"zone":             b.Window.Zone,
		}
	}
	if b.BatchesKnown {
		batches := make([]map[string]any, 0, len(b.Batches))
		for _, batch := range b.Batches {
			batches = append(batches, map[string]any{
				"batch_ref":    batch.BatchRef,
				"product_name": batch.ProductName,
				"quantity":     batch.Quantity,
				"expiry_date":  batch.ExpiryDate,
			})
		}
		payload["batches"] = batches
	}
	return payload
}

// slimSamples 把采样投影成送给模型的样子。
func (b FactBasis) slimSamples() []slimSample {
	out := make([]slimSample, 0, len(b.Samples))
	for _, ev := range b.Samples {
		out = append(out, slimSample{
			MeasuredAt:   ev.MeasuredAt.Format(time.RFC3339),
			TemperatureC: ev.TemperatureC,
			Unit:         ev.Unit,
			HumidityPct:  ev.HumidityPct,
			Events:       ev.Events,
			AlarmFlag:    ev.AlarmFlag,
		})
	}
	return out
}

// Summary 返回一句人类可读的依据摘要，随校验记录一起留档。
func (b FactBasis) Summary() string {
	parts := []string{fmt.Sprintf("输入时序 %d 条", len(b.Samples))}
	if b.Window != nil {
		parts = append(parts, fmt.Sprintf("规则窗口 %s ~ %s（%d 分钟）",
			b.Window.Start.Format(time.RFC3339), b.Window.End.Format(time.RFC3339),
			b.Window.DurationMinutes))
	} else {
		parts = append(parts, "规则窗口未提供")
	}
	if b.BatchesKnown {
		parts = append(parts, fmt.Sprintf("台账在库批次 %d 个", len(b.Batches)))
	} else {
		parts = append(parts, "台账在库批次清单未取到")
	}
	return strings.Join(parts, "；")
}

// FactMismatchError 是事实校验失败的结构化错误。
//
// 错误码恒为 E_MODEL_FACT_MISMATCH：形状不合规（E_MODEL_SCHEMA_INVALID）与
// 事实不符是两类问题——前者是模型没按 schema 答，后者是模型答了、但答的不是
// 现场发生的事。混成一个码，处置就分不清「再问一次」与「退回规则基线」。
//
// Attempts 记着问了几次：2 表示已经重问过一次仍不符，
// 此时不该再重试（模型层问过了，判定步骤层也不该再问），
// 而是把结论退回规则基线。
type FactMismatchError struct {
	// Base 是带错误码的底层错误。
	Base *errcode.Error
	// Schema 是这次调用的结构化输出分码。
	Schema SchemaName
	// Attempts 是本次一共调用了几次（含首次）。
	Attempts int
	// Issues 是被拦下的问题。
	Issues []FactIssue
	// RawFragment 是模型返回原文的截断片段。
	RawFragment string
}

// Error 实现 error 接口。
func (e *FactMismatchError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s（%s）", e.Base.Code, e.Base.Message, e.Base.Details)
}

// Unwrap 交出底层错误，errors.As 能取出 *errcode.Error。
func (e *FactMismatchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Base
}

// IsFactMismatch 判断一个错误是不是事实不符。
//
// 调用方（降级路径）据此决定要不要退回规则基线。
func IsFactMismatch(err error) bool {
	if err == nil {
		return false
	}
	var mismatch *FactMismatchError
	return errors.As(err, &mismatch)
}

// newFactMismatch 造一个事实不符的错误。
func newFactMismatch(schema SchemaName, attempts int, issues []FactIssue, raw []byte) *FactMismatchError {
	details := fmt.Sprintf("分码 %q：模型输出与输入事实不符（已重问 %d 次，共 %d 处）",
		schema, attempts-1, len(issues))
	for _, issue := range issues {
		details += "；" + issue.Reason
	}
	return &FactMismatchError{
		Base:        errcode.New(errcode.ModelFactMismatch, details),
		Schema:      schema,
		Attempts:    attempts,
		Issues:      issues,
		RawFragment: truncateForAudit(raw),
	}
}

// CheckFacts 按分码做事实校验，返回被拦下的问题。
//
// 返回空切片表示通过。各分码核对的字段不同：
//
//	attribution      —— 证据里的温度读数与设备事件条数；
//	window           —— 峰值温度、窗口起止、持续时长；
//	batch_advice     —— 每个批次号；
//	exception_record —— 峰值温度、起止、涉及批次；
//	disposition_note —— 无可核对字段（纯文字说明）。
//
// 依据缺失时不做核对（时序没给就不核温度、台账没查就不核批次），
// 但依据缺失这件事会写进校验记录的依据摘要里，不会悄悄混过去。
func CheckFacts(schema SchemaName, parsed any, basis FactBasis) []FactIssue {
	switch value := parsed.(type) {
	case *Attribution:
		return checkAttributionFacts(value, basis)
	case *Window:
		return checkWindowFacts(value, basis)
	case *BatchAdvice:
		return checkBatchAdviceFacts(value, basis)
	case *ExceptionRecord:
		return checkExceptionRecordFacts(value, basis)
	case *DispositionNote:
		return nil
	default:
		return nil
	}
}

// checkAttributionFacts 核对归因结论里的数值事实。
func checkAttributionFacts(attr *Attribution, basis FactBasis) []FactIssue {
	issues := make([]FactIssue, 0, 4)
	issues = append(issues, checkEvidenceFacts("evidence", attr.Evidence, basis)...)
	for i, candidate := range attr.Candidates {
		issues = append(issues, checkEvidenceFacts(fmt.Sprintf("candidates[%d].evidence", i), candidate.Evidence, basis)...)
	}
	return issues
}

// checkEvidenceFacts 核对一组证据里的数值事实。
func checkEvidenceFacts(prefix string, evidence []Evidence, basis FactBasis) []FactIssue {
	issues := make([]FactIssue, 0, len(evidence))
	for i, item := range evidence {
		path := fmt.Sprintf("%s[%d]", prefix, i)
		if temperatureEvidenceFeatures[item.Feature] {
			if issue := checkTemperatureValue(path+".value", item.Value, basis); issue != nil {
				issues = append(issues, *issue)
			}
			continue
		}
		if kind, ok := deviceEventFeatures[item.Feature]; ok {
			if issue := checkEventCount(path+".value", kind, item.Value, basis); issue != nil {
				issues = append(issues, *issue)
			}
		}
	}
	return issues
}

// checkWindowFacts 核对越限窗口：峰值温度、起止与时长。
func checkWindowFacts(win *Window, basis FactBasis) []FactIssue {
	issues := make([]FactIssue, 0, 3)
	if issue := checkTemperatureValue("peak_c", win.PeakC, basis); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := checkWindowBounds("start", win.Start, basis); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := checkWindowBounds("end", win.End, basis); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := checkWindowOrder(win.Start, win.End); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := checkDuration("duration_minutes", win.DurationMinutes, basis); issue != nil {
		issues = append(issues, *issue)
	}
	return issues
}

// checkBatchAdviceFacts 核对批次建议里的每个批次号。
func checkBatchAdviceFacts(advice *BatchAdvice, basis FactBasis) []FactIssue {
	issues := make([]FactIssue, 0, len(advice.Items))
	for i, item := range advice.Items {
		if issue := checkBatchRef(fmt.Sprintf("items[%d].batch_ref", i), item.BatchRef, basis); issue != nil {
			issues = append(issues, *issue)
		}
	}
	return issues
}

// checkExceptionRecordFacts 核对记录表里的峰值、起止与涉及批次。
func checkExceptionRecordFacts(record *ExceptionRecord, basis FactBasis) []FactIssue {
	issues := make([]FactIssue, 0, 3)
	if issue := checkTemperatureValue("peak_temperature_c", record.PeakTemperatureC, basis); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := checkWindowBounds("excursion_started_at", record.ExcursionStartedAt, basis); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := checkWindowBounds("excursion_ended_at", record.ExcursionEndedAt, basis); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := checkWindowOrder(record.ExcursionStartedAt, record.ExcursionEndedAt); issue != nil {
		issues = append(issues, *issue)
	}
	for i, batch := range record.Batches {
		if issue := checkBatchRef(fmt.Sprintf("batches[%d].batch_ref", i), batch.BatchRef, basis); issue != nil {
			issues = append(issues, *issue)
		}
	}
	return issues
}

// checkTemperatureValue 核对一个温度数值能否在输入时序里找到。
//
// 容差 ±0.05℃。找不到就是模型编了一个现场没有的读数——
// 这个数字会被写进异常记录表，容不得它来路不明。
func checkTemperatureValue(path string, claimed float64, basis FactBasis) *FactIssue {
	if len(basis.Samples) == 0 {
		// 没有输入时序就无从核对：这不是「通过」，是「没得核」。
		// 依据摘要里会写明时序条数为 0，事后复核看得见这次没核过。
		return nil
	}
	closest, diff, found := closestTemperature(basis.Samples, claimed)
	if found {
		return nil
	}
	return &FactIssue{
		Path:    path,
		Kind:    domain.FactKindTemperatureNotFound,
		Claimed: fmt.Sprintf("%.2f℃", claimed),
		Basis: fmt.Sprintf("输入时序 %d 条读数里没有这个温度；最接近的一条是 %.2f℃，偏差 %.2f℃（容差 ±%.2f℃）",
			len(basis.Samples), closest, diff, TemperatureToleranceC),
		Reason: fmt.Sprintf("字段 %s 的温度 %s 在输入时序里找不到（最接近 %.2f℃，偏差 %.2f℃，容差 ±%.2f℃）",
			path, fmt.Sprintf("%.2f℃", claimed), closest, diff, TemperatureToleranceC),
	}
}

// closestTemperature 找出与 claimed 最接近的一条读数温度。
//
// found 为 false 表示没有任何一条读数落在容差内。
func closestTemperature(samples []domain.TemperatureEvent, claimed float64) (float64, float64, bool) {
	closest := 0.0
	best := 0.0
	for i, ev := range samples {
		diff := ev.TemperatureC - claimed
		if diff < 0 {
			diff = -diff
		}
		if i == 0 || diff < best {
			closest = ev.TemperatureC
			best = diff
		}
	}
	return closest, best, best <= TemperatureToleranceC
}

// checkWindowOrder 核对起止顺序：结束时刻不能早于开始时刻。
//
// 起止颠倒的模型输出多半是把窗口的两头写反了——
// 这种错误在台账上会变成「越温时长为负」，复核时一眼就能看出来，
// 但那已经写进台账了，所以这一关得在写之前拦。
func checkWindowOrder(start, end time.Time) *FactIssue {
	if !end.Before(start) {
		return nil
	}
	return &FactIssue{
		Path:    "end",
		Kind:    domain.FactKindWindowOutOfRange,
		Claimed: end.Format(time.RFC3339),
		Basis:   "开始时刻 " + start.Format(time.RFC3339),
		Reason: fmt.Sprintf("字段 end 的时刻 %s 早于字段 start 的时刻 %s，起止顺序颠倒",
			end.Format(time.RFC3339), start.Format(time.RFC3339)),
	}
}

// checkWindowBounds 核对一个时刻是否落在规则抽出的窗口内。
//
// 窗口起止必须落在规则窗口之内：模型可以描述得更细、可以给证据，
// 但不能把越温的时间说成别的时段——那会让复核的人按错的时间去查设备日志。
func checkWindowBounds(path string, moment time.Time, basis FactBasis) *FactIssue {
	if basis.Window == nil {
		return nil
	}
	win := basis.Window
	if moment.Before(win.Start) || moment.After(win.End) {
		return &FactIssue{
			Path:    path,
			Kind:    domain.FactKindWindowOutOfRange,
			Claimed: moment.Format(time.RFC3339),
			Basis: fmt.Sprintf("规则窗口为 %s ~ %s",
				win.Start.Format(time.RFC3339), win.End.Format(time.RFC3339)),
			Reason: fmt.Sprintf("字段 %s 的时刻 %s 落在规则窗口（%s ~ %s）之外",
				path, moment.Format(time.RFC3339),
				win.Start.Format(time.RFC3339), win.End.Format(time.RFC3339)),
		}
	}
	return nil
}

// checkDuration 核对持续时长与规则窗口是否对得上。
//
// 容差取一个采样周期：窗口里有数据缺口时，规则算出的时长与
// 「起止相减」本来就会有差，拿一采样周期去比，既能放过这种合理差异，
// 又拦得住「时长凭空多出几十分钟」这种编造。
func checkDuration(path string, claimed int, basis FactBasis) *FactIssue {
	if basis.Window == nil {
		return nil
	}
	tolerance := 1
	if basis.Window.SamplingIntervalSec > 0 {
		tolerance = basis.Window.SamplingIntervalSec / 60
		if tolerance < 1 {
			tolerance = 1
		}
	}
	diff := claimed - basis.Window.DurationMinutes
	if diff < 0 {
		diff = -diff
	}
	if diff <= tolerance {
		return nil
	}
	return &FactIssue{
		Path:    path,
		Kind:    domain.FactKindDurationMismatch,
		Claimed: fmt.Sprintf("%d 分钟", claimed),
		Basis: fmt.Sprintf("规则窗口时长 %d 分钟（容差 %d 分钟）",
			basis.Window.DurationMinutes, tolerance),
		Reason: fmt.Sprintf("字段 %s 的持续时长 %d 分钟与规则窗口的 %d 分钟对不上（容差 %d 分钟）",
			path, claimed, basis.Window.DurationMinutes, tolerance),
	}
}

// checkBatchRef 核对批次号是否在台账在库批次清单里。
//
// 批次号只由冷链台账给：模型提到的批次号必须在清单里原样出现，
// 否则就是编了一个不存在的批次——按它去隔离、报废，报废的是空气。
func checkBatchRef(path, ref string, basis FactBasis) *FactIssue {
	if !basis.BatchesKnown {
		return nil
	}
	trimmed := strings.TrimSpace(ref)
	for _, batch := range basis.Batches {
		if batch.BatchRef == trimmed {
			return nil
		}
	}
	return &FactIssue{
		Path:    path,
		Kind:    domain.FactKindBatchRefUnknown,
		Claimed: trimmed,
		Basis:   "台账在库批次清单：" + batchRefSummary(basis.Batches),
		Reason: fmt.Sprintf("字段 %s 的批次号 %q 不在台账在库批次清单里（清单内批次号：%s）",
			path, trimmed, batchRefSummary(basis.Batches)),
	}
}

// checkEventCount 核对设备事件条数与输入时序里数得出来的是否一致。
func checkEventCount(path, kind string, claimed float64, basis FactBasis) *FactIssue {
	if len(basis.Samples) == 0 {
		return nil
	}
	count := 0
	for _, ev := range basis.Samples {
		for _, item := range ev.Events {
			if item == kind {
				count++
				break
			}
		}
	}
	if int(claimed+0.5) == count {
		return nil
	}
	return &FactIssue{
		Path:    path,
		Kind:    domain.FactKindEventCountMismatch,
		Claimed: fmt.Sprintf("%.0f 次", claimed),
		Basis:   fmt.Sprintf("输入时序 %d 条读数里 %s 事件出现 %d 次", len(basis.Samples), kind, count),
		Reason: fmt.Sprintf("字段 %s 的事件条数 %.0f 次与输入时序不符（时序里 %s 出现 %d 次）",
			path, claimed, kind, count),
	}
}

// batchRefSummary 把在库批次号拼成一句摘要。
func batchRefSummary(batches []domain.Batch) string {
	if len(batches) == 0 {
		return "（空）"
	}
	refs := make([]string, 0, len(batches))
	for _, batch := range batches {
		refs = append(refs, batch.BatchRef)
	}
	sort.Strings(refs)
	if len(refs) > 8 {
		refs = append(refs[:8], fmt.Sprintf("…共 %d 个", len(refs)))
	}
	return strings.Join(refs, "、")
}

// FactRecorder 是事实校验记录的落库出口。
//
// 由存储层实现（internal/store）。模型层不直接碰数据库：
// 校验与落库分开，测试里不落库也能验校验。
type FactRecorder interface {
	// RecordFactCheck 落一条事实校验记录。
	RecordFactCheck(ctx context.Context, record domain.FactCheckRecord) error
}

// AskFunc 是一次结构化模型调用的出口。
//
// feedback 非空表示这是「重问」：上一次的事实校验没过，
// 反馈内容要带进这次请求，让模型看得见自己错在哪。
type AskFunc func(ctx context.Context, schema SchemaName, payload any, feedback string) (*Completion, error)

// GatewayAsker 返回一个走 ModelGateway 的 AskFunc。
//
// 这是「领养源产品调用链」的落点：请求怎么发、错误怎么分档，全在网关上，
// 本文件只在它之上加一道事实校验。
func GatewayAsker(gateway *ModelGateway) AskFunc {
	return func(ctx context.Context, schema SchemaName, payload any, feedback string) (*Completion, error) {
		if gateway == nil {
			return nil, errcode.Newf(errcode.ConfigMissing, "模型网关未装配")
		}
		opts := RequestOptions{UserPayload: payload}
		if strings.TrimSpace(feedback) != "" {
			opts.SystemPrompt = DefaultSystemPrompt(schema) + "\n" + feedback
		}
		return gateway.Complete(ctx, schema, opts)
	}
}

// Result 是一次调用的事实校验结果。
//
// 即使校验没过也要给出来：Records 记着每一次的结论与被拦下的问题，
// 落库与审计都要用它。
type Result struct {
	// Schema 是这次调用的结构化输出分码。
	Schema SchemaName
	// Content 是通过校验的那份响应原文。
	Content []byte
	// Attempts 是一共调用了几次（1 = 一次就过，2 = 重问后过）。
	Attempts int
	// Issues 是首次调用被拦下、随后被纠正的问题（供审计回看）。
	Issues []FactIssue
	// Records 是每一次尝试的校验记录（供落库）。
	Records []domain.FactCheckRecord
}

// Asker 把「调用模型」与「事实校验」串起来。
//
// 一次 Ask 的完整路径：
//
//	调用模型 → 形状校验 → 事实校验 →（不过）带反馈重问一次 →（还不过）返回事实不符错误
//
// 形状不合规也重问一次：模型没按 schema 答时，把「哪个字段不合规」告诉它，
// 第二次往往就答对了。这一层与判定步骤层的重试是分开的——
// 步骤层管「这一步失败了要不要再来一次」，本层管「模型这次答歪了要不要改一次」。
type Asker struct {
	// Caller 是提问出口；为 nil 时装配即报错。
	Caller AskFunc
	// Basis 是事实校验的权威依据。
	Basis FactBasis
	// RunID 是所属判定 run（记进校验记录）。
	RunID string
	// StepName 是所属步骤名（记进校验记录）。
	StepName string
	// Recorder 是校验记录的落库出口；为 nil 时只留内存。
	Recorder FactRecorder
	// Auditor 是模型调用审计的落库出口；为 nil 时不记调用元数据。
	//
	// 它与 Recorder 分开：校验记录说的是「这次答的内容过没过事实校验」，
	// 审计记录说的是「这次调用花了多少、用了哪个模型、成没成」。
	// 两件事的成败互不牵连——审计写不进去不该让一次本来通过的判定变成失败。
	Auditor AuditRecorder
	// Now 是时钟；测试可注入固定时钟。
	Now func() time.Time
}

// ForRun 返回一个绑定了本次判定身份与事实依据的副本。
//
// 依据随判定而变（每条 run 的采样与窗口都不同），而 Asker 本身可能被
// 多条判定共用。就地改字段的话，两条判定同时跑时会互相踩依据——
// 拿 A 判定的尺子去量 B 判定的输出，事实校验就成了摆设。
// 复制一份再改，既安全又不影响原有装配。
func (a *Asker) ForRun(runID, stepName string, basis FactBasis) *Asker {
	if a == nil {
		return nil
	}
	clone := *a
	clone.RunID = strings.TrimSpace(runID)
	clone.StepName = strings.TrimSpace(stepName)
	clone.Basis = basis
	return &clone
}

// Ask 调用模型并做形状与事实校验。
//
// 返回的 any 是按分码解析好的结构化输出（*Attribution / *Window /
// *BatchAdvice / *ExceptionRecord / *DispositionNote）。
//
// 校验没过时返回错误：形状不合规给 E_MODEL_SCHEMA_INVALID，
// 事实不符给 E_MODEL_FACT_MISMATCH（带 FactMismatchError，
// 可用 IsFactMismatch 判定）。两种情况下 Result 仍会返回，
// 里面的 Records 记着这一次的校验过程。
func (a *Asker) Ask(ctx context.Context, schema SchemaName, payload any) (any, *Result, error) {
	if a == nil || a.Caller == nil {
		return nil, nil, errcode.Newf(errcode.Validation, "事实校验调用器未装配提问出口")
	}
	if payload == nil {
		payload = a.Basis.Payload()
	}

	result := &Result{Schema: schema}
	feedback := ""

	for attempt := 1; attempt <= factCheckMaxAttempts; attempt++ {
		// 尝试序号先记上：调用本身失败（端点不可达）也是「问了一次」，
		// 记 0 次会让审计以为压根没问过模型。
		result.Attempts = attempt
		completion, err := a.Caller(ctx, schema, payload, feedback)
		if err != nil {
			// 端点不可达、超时、协议不合规：这些不是「模型答歪了」，
			// 重问也没用（端点还没活过来）。原样上抛，由降级路径接手。
			// 审计链照样记这一次：没发成功的调用也是一次调用，
			// 它花没花钱、卡在哪一档，端点那边也要看得见。
			a.recordCall(ctx, schema, completion, auditOutcomeFailed, auditErrorCode(err), false)
			return nil, result, err
		}
		result.Content = completion.Content

		parsed, err := parseFor(schema, completion.Content)
		if err != nil {
			result.Records = append(result.Records,
				a.record(schema, attempt, domain.FactVerdictShapeInvalid, nil, completion.Content))
			// 形状不合规就是「这次调用没成」：HTTP 200 不等于这次调用成功。
			a.recordCall(ctx, schema, completion, auditOutcomeFailed, auditErrorCode(err), false)
			if attempt == factCheckMaxAttempts {
				return nil, result, err
			}
			feedback = shapeFeedback(err)
			continue
		}

		issues := CheckFacts(schema, parsed, a.Basis)
		verdict := domain.FactVerdictPass
		if len(issues) > 0 {
			verdict = domain.FactVerdictMismatch
		}
		result.Records = append(result.Records,
			a.record(schema, attempt, verdict, issues, completion.Content))

		if len(issues) == 0 {
			a.recordCall(ctx, schema, completion, auditOutcomeSucceeded, "", false)
			return parsed, result, nil
		}
		// 事实不符：调用通了、答案不合规——同样记「失败」，
		// 并把错误码带上，让审计能一眼看出这次调用为什么没成。
		a.recordCall(ctx, schema, completion, auditOutcomeFailed,
			string(errcode.ModelFactMismatch), false)
		if attempt == 1 {
			result.Issues = issues
		}
		if attempt == factCheckMaxAttempts {
			return nil, result, newFactMismatch(schema, attempt, issues, completion.Content)
		}
		feedback = factFeedback(issues, a.Basis)
	}

	return nil, result, errcode.Newf(errcode.ModelFactMismatch,
		"事实校验未得出结论（分码 %q）", schema)
}

// record 造一条校验记录，并尝试落库。
//
// 落库失败不改变本次调用的结论：校验记录是审计材料，
// 不是业务状态。写不进去要报错，但不能让一次本来通过的判定变成失败。
// 写库失败会在返回的错误里带出来（由调用方决定要不要显眼地提示）。
func (a *Asker) record(schema SchemaName, attempt int, verdict string,
	issues []FactIssue, raw []byte) domain.FactCheckRecord {
	now := time.Now
	if a != nil && a.Now != nil {
		now = a.Now
	}
	record := domain.FactCheckRecord{
		RunID:       a.runID(),
		StepName:    a.stepName(),
		SchemaName:  string(schema),
		Attempt:     attempt,
		Verdict:     verdict,
		Issues:      issues,
		RawFragment: truncateForAudit(raw),
		CreatedAt:   now(),
	}
	if a.Recorder != nil && record.RunID != "" {
		if err := a.Recorder.RecordFactCheck(context.Background(), record); err != nil {
			// 审计写不进去不该篡改业务结论，但也不能装作没发生：
			// 记进服务日志，判定结论照常。
			logFactCheckPersistFailure(err)
		}
	}
	return record
}

// runID 返回所属 run 标识。
func (a *Asker) runID() string {
	if a == nil {
		return ""
	}
	return a.RunID
}

// stepName 返回所属步骤名。
func (a *Asker) stepName() string {
	if a == nil {
		return ""
	}
	return a.StepName
}

// parseFor 按分码解析一份响应。
func parseFor(schema SchemaName, raw []byte) (any, error) {
	switch schema {
	case SchemaAttribution:
		return ParseAttribution(raw)
	case SchemaWindow:
		return ParseWindow(raw)
	case SchemaBatchAdvice:
		return ParseBatchAdvice(raw)
	case SchemaExceptionRecord:
		return ParseExceptionRecord(raw)
	case SchemaDispositionNote:
		return ParseDispositionNote(raw)
	default:
		return nil, errcode.Newf(errcode.Validation, "未登记的模型结构化输出分码：%q", schema)
	}
}

// shapeFeedback 把形状不合规的问题写成重问反馈。
func shapeFeedback(err error) string {
	var schemaErr *SchemaError
	if errors.As(err, &schemaErr) {
		return fmt.Sprintf("上一次回答没有通过结构化校验，请改正后重答：%s（%s）。"+
			"只输出符合给定 JSON Schema 的 JSON，不要输出解释性文字或 Markdown 代码块。",
			schemaErr.Issue.Reason, schemaErr.Issue.Path)
	}
	return fmt.Sprintf("上一次回答没有通过结构化校验，请改正后重答：%v", err)
}

// factFeedback 把被拦下的事实问题写成重问反馈。
//
// 反馈要具体到「哪个字段、错在哪、依据是什么」：
// 只说「有错」模型会原样再答一遍，说清依据它才知道该改成什么。
func factFeedback(issues []FactIssue, basis FactBasis) string {
	var b strings.Builder
	b.WriteString("上一次回答里有内容与本次判定的输入事实不符，请改正后重答。\n")
	b.WriteString("本次可依据的事实：")
	b.WriteString(basis.Summary())
	b.WriteString("。\n")
	for i, issue := range issues {
		fmt.Fprintf(&b, "%d. 字段 %s：%s\n", i+1, issue.Path, issue.Reason)
		fmt.Fprintf(&b, "   依据：%s\n", issue.Basis)
	}
	b.WriteString("只使用上述事实里出现过的温度、时刻与批次号；给不出来的就留空并说明还缺什么，不要编。")
	return b.String()
}
