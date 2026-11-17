// 本文件装配判定步骤链：取数 → 抽越限窗口 → 归因 → 成稿记录表 → 停在待主管确认。
//
// 装配与执行分开放：执行器（executor.go）只管「按顺序跑、把产物按分码存下来、
// 失败时不推进状态」，它不认识「温区」也不认识「批次」；
// 本文件认识这些业务概念，负责把每一步该做什么说清楚。
//
// 分工带来的好处：换一个能力（规则基线归因、模型归因、批次建议、批次清单读侧）
// 只需要换掉对应那一步的处理器，步骤链的顺序、产物分码与落库方式都不动。
//
// 现在装配的五步各自是什么：
//   - 取数：把这次判定依据的温度采样取到手（随判定传入或从库里按窗口区间回读）。
//   - 抽窗口：按点位温区比较温度，抽出越温窗口，并记下温区来源。
//   - 归因：给出原因结论与证据。默认用规则基线归因（温升速率、湿度、
//     开门时长与滞后、断电除霜、读数突变与恒定，综合成带证据的候选集）；
//     模型归因装配后由外部传入。
//   - 成稿：组装《温度异常情况记录表》的结构化内容。
//     批次与处置此刻拿不到（批次号只能由冷链台账给，处置要主管确认），
//     就把它们显式列为待补，不填一个像样的值。
//   - 待确认：结论就绪，run 停在待药学部主管确认，不自动写台账。
package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/drafting"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/model"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// SampleSource 是采样来源（通常是本地库）。
type SampleSource interface {
	// EventsInRange 按点位与时间区间回读温度采样，区间两端闭合。
	EventsInRange(ctx context.Context, pointID string, from, to time.Time) ([]domain.TemperatureEvent, error)
}

// BatchReader 是在库批次读侧（冷链台账）的最小接口。
//
// 它只回一个查询结果形状（domain.BatchLookup），不回裸错误：
// 查询结果里已经带着「查到了没有、为什么没查成」，再叠一个错误
// 会让调用方有机会把「没查成」当成「查到了但是空的」——那正是最危险的一种误读。
type BatchReader interface {
	// Batches 按点位与时刻查在库批次清单。
	Batches(ctx context.Context, pointID string, at time.Time) (domain.BatchLookup, error)
}

// ChainDeps 是判定步骤链的外部依赖。
type ChainDeps struct {
	// Profile 是点位温区配置。缺它整条链装不起来：系统不猜温区、
	// 不用默认值顶替，宁可这条判定转人工。
	Profile *domain.PointProfile
	// Samples 是已经取到手的采样。有它就不必再查来源。
	Samples []domain.TemperatureEvent
	// Batches 是本次判定时在库的批次清单。
	//
	// 批次号只由冷链台账给出：给了它，判定才能把在库批次挂到各自的制品温区下，
	// 成稿也才能逐批次给可用性建议（哪批受影响、哪批没受影响，要分开说）。
	// 不给（台账还没接上、或本次判定不涉批次）时判定照常做，只是不做批次映射。
	Batches []domain.Batch
	// BatchReader 是在库批次的读侧（冷链台账）。
	//
	// 本地给了 Batches 时以本地的为准；没给、而读侧接上了时，
	// 成稿那一步会按越温窗口起点去冷链台账查在库批次。
	//
	// 查不到不是判定失败：台账不可达时成稿写「批次信息缺失，待人工核对」，
	// 结论照样停在待主管确认——越温窗口与归因结论不受库存查询影响，
	// 而一个编出来的批次号会直接害掉那批货。
	BatchReader BatchReader
	// Source 是采样来源（通常是本地库）。Samples 为空时用它按窗口区间取数。
	Source SampleSource
	// Attribute 可选：自定义归因处理器（规则基线 / 模型归因装配后由外部传入）。
	Attribute StepHandler
	// AuditRecorder 可选：模型调用审计的落库出口。
	//
	// 装配模型归因时由它把「用了哪个模型、花了多少 token、成本多少、
	// 成没成」记进审计链，供内审与飞检经 GET /v1/runs/{id}/audit 调阅。
	// 为 nil 时不记调用元数据，判定照常跑。
	AuditRecorder audit.AuditRecorder
	// Draft 可选：自定义成稿处理器。
	Draft StepHandler
	// Now 可选：时钟；测试可注入固定时钟。
	Now func() time.Time
}

// NewChain 装配判定步骤链。
//
// 返回的切片顺序即执行顺序：取数 → 抽窗口 → 归因 → 成稿 → 待确认。
func NewChain(deps ChainDeps) ([]StepSpec, error) {
	if deps.Profile == nil {
		return nil, errcode.Newf(errcode.Validation,
			"缺少点位温区配置，步骤链无法装配（系统不猜温区、不用默认值顶替）")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	fetch := func(ctx context.Context, in StepInput) (any, error) {
		return fetchSeries(ctx, in, deps)
	}
	extract := func(ctx context.Context, in StepInput) (any, error) {
		return extractWindow(ctx, in, deps)
	}
	attribute := deps.Attribute
	if attribute == nil {
		attribute = func(ctx context.Context, in StepInput) (any, error) {
			return rulesBaselineAttribution(ctx, in, deps)
		}
	}
	draft := deps.Draft
	if draft == nil {
		draft = func(ctx context.Context, in StepInput) (any, error) {
			return draftExceptionRecord(ctx, in, now, deps)
		}
	}
	await := func(ctx context.Context, in StepInput) (any, error) {
		return awaitConfirmation(ctx, in, now)
	}

	return []StepSpec{
		{
			Name:        StepFetchSeries,
			PayloadKind: domain.PayloadSeries,
			Handler:     fetch,
		},
		{
			Name:        StepExtractWindow,
			PayloadKind: domain.PayloadWindow,
			Handler:     extract,
			OnSuccess: func(run *domain.Run, product any) error {
				// 状态推进只走 domain.Run 的写入口：合法迁移表、终态保护与迁移留痕
				// 都在 workflow 包里，绕开它就查不到「这条判定是怎么走到这个状态的」。
				// 用 EnsureState：窄判定路径已经给出结论时，这一步再跑一遍属于重做，
				// 不是非法迁移。
				_, err := run.EnsureState(workflow.Request{
					To:     workflow.StateAssessed,
					Action: workflow.ActionAuto,
					Reason: "excursion_window_extracted",
					Note:   "越温窗口已抽出，给出可核对的判定结论",
					Actor:  workflow.ActorSystem,
				})
				return err
			},
		},
		{
			Name:        StepAttribute,
			PayloadKind: domain.PayloadAttribution,
			Handler:     attribute,
			OnSuccess: func(run *domain.Run, product any) error {
				// 降级标记要跟着 run 走：查一条判定的人第一眼看的是 run 的状态，
				// 不该非要去翻步骤产物才知道这条结论是退回来的。
				// 这一步不改业务状态——降级不改变「结论还没等人确认」这件事。
				payload, ok := product.(*domain.AttributionPayload)
				if !ok || !payload.Degraded {
					return nil
				}
				run.Degraded = true
				run.DegradeReason = payload.DegradeReason
				return nil
			},
		},
		{
			Name:        StepDraft,
			PayloadKind: domain.PayloadExceptionRecord,
			Handler:     draft,
		},
		{
			Name:        StepAwaitConfirmation,
			PayloadKind: domain.PayloadConfirmationReady,
			Handler:     await,
			OnSuccess: func(run *domain.Run, product any) error {
				// 判定链只到「待确认」为止：再往后（已确认 / 已写回）由主管的确认动作推进，
				// 模型产出不得直接推进业务状态。
				_, err := run.EnsureState(workflow.Request{
					To:     workflow.StateAwaitingConfirmation,
					Action: workflow.ActionAuto,
					Reason: "draft_ready_for_confirmation",
					Note:   "异常记录表已成稿，结论停在待药学部主管确认，未触发出站写回",
					Actor:  workflow.ActorSystem,
				})
				return err
			},
		},
	}, nil
}

// ChainStepNames 返回步骤链的步骤名（按执行顺序）。
func ChainStepNames() []string {
	return []string{
		StepFetchSeries,
		StepExtractWindow,
		StepAttribute,
		StepDraft,
		StepAwaitConfirmation,
	}
}

// fetchSeries 取数：把这次判定依据的温度采样取到手。
//
// 优先用随判定传入的采样（调用方已经拿着）；没有就从来源按窗口区间回读。
// 两种取法都按闭区间取，边界少一条事后复算就会对不上当初的窗口。
func fetchSeries(ctx context.Context, in StepInput, deps ChainDeps) (any, error) {
	run := in.Run
	samples := deps.Samples
	var from, to time.Time

	if len(samples) > 0 {
		from, to = sampleRange(samples)
	} else {
		if deps.Source == nil {
			return nil, errcode.Newf(errcode.Validation,
				"没有采样来源：既未随判定传入采样，也没有可查询的采样来源")
		}
		if run.WindowStart == nil || run.WindowEnd == nil {
			return nil, errcode.Newf(errcode.Validation,
				"run %s 未绑定越温窗口，无法确定取数区间", run.RunID)
		}
		from, to = *run.WindowStart, *run.WindowEnd
		got, err := deps.Source.EventsInRange(ctx, run.PointID, from, to)
		if err != nil {
			return nil, err
		}
		samples = got
	}

	if len(samples) == 0 {
		return nil, errcode.Newf(errcode.NotFound,
			"run %s 在 %s ~ %s 区间内没有取到温度采样",
			run.RunID, from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	return &domain.SeriesPayload{
		PointID:     run.PointID,
		From:        from,
		To:          to,
		SampleCount: len(samples),
		Samples:     samples,
	}, nil
}

// extractWindow 抽越限窗口：按点位温区比较温度，抽出窗口并记下温区来源。
func extractWindow(ctx context.Context, in StepInput, deps ChainDeps) (any, error) {
	series, ok := in.Product(domain.PayloadSeries)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "取数步骤没有产出采样，无法抽取越温窗口")
	}
	payload, ok := series.(*domain.SeriesPayload)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "取数产物类型不符：%T", series)
	}

	// 温区闸门：把点位配置与这批采样绑定起来，得出温区、出处、精度与设备归属。
	// 这一步失败（配置缺失、点位对不上、设备归属歧义）时不产出窗口——
	// 窗口是按温区算出来的，温区不可信时窗口也不可信。
	//
	// 在库批次一并带上：批次要挂到各自的制品温区下，成稿才谈得上
	// 「哪一批受影响、哪一批没受影响」。
	zone, err := rules.ResolveZoneForEvent(deps.Profile, payload.Samples[0], deps.Batches)
	if err != nil {
		return nil, err
	}
	window, err := rules.ExtractWindow(payload.Samples, zone)
	if err != nil {
		return nil, err
	}
	if window == nil {
		// 没窗口不等于没越限：可能是采样全在温区内，也可能是越了但没够最短时长门槛。
		// 两种情形给同一句报错，复核的人会以为压根没发生过异常。
		summary, err := rules.SummarizeExcursions(payload.Samples, zone)
		if err != nil {
			return nil, err
		}
		if summary.SegmentCount > 0 {
			msg := fmt.Sprintf(
				"取到的 %d 条采样中有 %d 条越限（%d 段），但确认时长 %d 分钟未达最短越限门槛 %d 分钟，没有越温窗口",
				len(payload.Samples), summary.OutOfRangeSamples, summary.SegmentCount,
				summary.LongestConfirmedMinutes, summary.MinExcursionMinutes)
			if summary.TransientSegmentCount > 0 {
				// 短时波动单列：它是「发生过但不够长」，不是「没发生过」。
				msg += fmt.Sprintf("；其中 %d 段属短时波动（最长 %d 分钟），已单独归集，不生成越温窗口",
					summary.TransientSegmentCount, summary.LongestTransientMinutes)
			}
			return nil, errcode.Newf(errcode.Validation, "%s", msg)
		}
		return nil, errcode.Newf(errcode.Validation,
			"取到的 %d 条采样均在温区内，没有越温窗口", len(payload.Samples))
	}

	return &domain.WindowPayload{
		Window:     *window,
		ZoneMinC:   zone.ZoneMinC,
		ZoneMaxC:   zone.ZoneMaxC,
		ZoneSource: zone.ZoneSource,
		ZoneBasis:  zone.ZoneBasis,
		Products:   zone.Products,
		Precision:  zone.Precision,
		Binding:    zone.Binding,
	}, nil
}

// rulesBaselineAttribution 是规则基线归因处理器。
//
// 它把这次判定的采样交给规则基线归因：先算归因特征（温升速率、湿度、
// 开门时长与滞后、断电除霜、读数突变与恒定），再合成带证据的候选集。
//
// 之所以放在判定步骤链里而不是只留在规则包里：规则基线不只是一个库函数，
// 它是模型不可达时的降级落点——模型挂了就退到这儿，结论停在待主管确认。
// 降级路径必须与在线路径走同一套判据，否则两条路的结论没法比。
func rulesBaselineAttribution(ctx context.Context, in StepInput, deps ChainDeps) (any, error) {
	series, ok := in.Product(domain.PayloadSeries)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "归因步骤缺少取数产物")
	}
	payload, ok := series.(*domain.SeriesPayload)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "取数产物类型不符：%T", series)
	}
	if len(payload.Samples) == 0 {
		return nil, errcode.Newf(errcode.Validation, "归因步骤的取数产物里没有采样")
	}
	windowValue, ok := in.Product(domain.PayloadWindow)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "归因步骤缺少越温窗口产物")
	}
	window, ok := windowValue.(*domain.WindowPayload)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "越温窗口产物类型不符：%T", windowValue)
	}

	// 温区与特征用的是同一份配置：特征里的峰值偏离度、湿度基线都要按温区算。
	zone, err := rules.ResolveZoneForEvent(deps.Profile, payload.Samples[0], nil)
	if err != nil {
		return nil, err
	}
	features, err := rules.ExtractFeatures(payload.Samples, &window.Window, zone)
	if err != nil {
		return nil, err
	}
	return rules.Attribute(features, rules.AttributionOptions{Window: &window.Window})
}

// draftExceptionRecord 由窗口与归因结论组装结构化异常记录表。
//
// 它只填此刻已经确定的字段：点位、设备、起止、峰值、持续时长、温区、
// 原因结论与证据。批次号只能由冷链台账给（系统不造新号），处置要由主管确认：
// 批次清单此刻拿不到就显式列为待补，而不是填一个像样的值。
//
// 处置建议给到「建议」这一层就停：逐批次算清楚受没受影响、建议怎么办，
// 但批次能不能用仍由疫苗生产企业评估质量影响、再由药学部主管确认。
func draftExceptionRecord(ctx context.Context, in StepInput, now func() time.Time, deps ChainDeps) (any, error) {
	series, ok := in.Product(domain.PayloadSeries)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "成稿步骤缺少取数产物")
	}
	seriesPayload, ok := series.(*domain.SeriesPayload)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "取数产物类型不符：%T", series)
	}
	windowValue, ok := in.Product(domain.PayloadWindow)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "成稿步骤缺少越温窗口产物")
	}
	window, ok := windowValue.(*domain.WindowPayload)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "越温窗口产物类型不符：%T", windowValue)
	}
	attributionValue, ok := in.Product(domain.PayloadAttribution)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "成稿步骤缺少归因结论产物")
	}
	attribution, ok := attributionValue.(*domain.AttributionPayload)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "归因产物类型不符：%T", attributionValue)
	}

	record := &domain.ExceptionRecordPayload{
		PointID:            in.Run.PointID,
		DeviceName:         deviceNameOf(seriesPayload.Samples),
		ExcursionStartedAt: window.Window.Start,
		ExcursionEndedAt:   window.Window.End,
		PeakTemperatureC:   window.Window.PeakC,
		DurationMinutes:    window.Window.DurationMinutes,
		Zone:               window.Window.Zone,
		CauseConclusion:    attribution.CauseCode,
		Evidence:           attribution.Evidence,
		GeneratedAt:        now(),
	}
	// 精度不足要透传：读数本身不可信时，结论也得显眼地标出来。
	for _, sample := range seriesPayload.Samples {
		if sample.Precision.Limited {
			record.PrecisionLimited = true
			break
		}
	}
	// 降级也要写进稿子里：这张表是要递到疫苗生产企业面前的，
	// 一条「开门导致」若没写明是模型不可达时退回来的，会被当成查实过的结论。
	if attribution.Degraded {
		record.Degraded = true
		if note := model.DegradeNote(attribution); note != "" {
			record.Notes = append(record.Notes, note)
		}
	}
	// 批次清单（来自冷链台账）：批次号只引用、不改写。
	//
	// 本地给了清单就用本地的；没给、而读侧接上了，就按越温窗口起点去台账查。
	// 查不到时这里不会编一个批次号出来——成稿会写明「批次信息缺失，待人工核对」。
	batches, lookup := resolveBatches(ctx, in.Run.PointID, window.Window.Start, deps)
	record.Batches = batches
	record.BatchLookup = lookup
	if lookup != nil && lookup.NeedsManual {
		record.Notes = append(record.Notes,
			domain.BatchMissingText+"："+lookup.Detail)
	}

	// 批次可用性建议：逐批次按自己的温区与暴露时长算，给到「建议」这一层。
	// 建议算不出来时（处置取值越界）就让这一步失败——一份处置结论可疑的稿子，
	// 不该以「成功」的样子留在库里。
	advice, err := drafting.Advise(drafting.AdviceInputs{
		Record:      record,
		Window:      window,
		Attribution: attribution,
		PointID:     in.Run.PointID,
		Lookup:      lookup,
	})
	if err != nil {
		return nil, err
	}
	record.Advice = &advice
	record.Disposition = advice.Disposition
	record.DispositionNote = advice.Note

	// 要素完整性校验：这份稿子齐不齐、差哪几项、哪几项挡着台账写回，
	// 结论随成稿产物一起落库（确认动作发生在判定之后，甚至跨进程重启之后，
	// 答案必须还在库里，不能靠人记得当时屏幕上出现过什么）。
	//
	// 缺项里「batches」在批次清单拿不到时出现；「disposition」始终出现——
	// 它指的是主管确认后的最终处置，建议已给出但最终处置还没定。
	quality := drafting.CheckQuality(drafting.Inputs{
		Record:      record,
		Series:      seriesPayload,
		Window:      window,
		Attribution: attribution,
	}, now())
	record.Quality = quality.Domain()
	record.MissingFields = quality.MissingFields
	record.NeedsReview = quality.NeedsReview()

	// 结构化字段是给机器读的，正文是给人读的：同一批数据渲染成
	// 《温度异常情况记录表》草稿，跟产物存在一起。
	// 成稿只读产物、不改产物，渲染失败就让这一步失败——
	// 一份渲染不出来的稿子，不该以「成功」的样子留在库里。
	draft, err := drafting.Render(drafting.Inputs{
		Record:      record,
		Series:      seriesPayload,
		Window:      window,
		Attribution: attribution,
		Advice:      record.Advice,
	}, drafting.Options{Now: now})
	if err != nil {
		return nil, err
	}
	record.DraftText = draft.Text
	return record, nil
}

// resolveBatches 取得本次判定要用的在库批次清单与这次查询的事实。
//
// 三种取法，按可信度从高到低：
//
//  1. 本地给了清单（命令行 --batches 传入的台账导出）：直接用，
//     不标查询状态——它不是从台账实时查来的，标成「台账已确认」是撒谎。
//  2. 接了台账读侧：按越温窗口起点去查，用查回来的批次与查回来的状态。
//  3. 两样都没有：没有批次，也没有查询事实——成稿照旧写「待补」。
//
// 台账没查成（第 2 种里的失败）不是判定失败：窗口与归因结论照样成立，
// 结论停在待主管确认，缺的那一栏在成稿上写明「批次信息缺失，待人工核对」。
// 一个编出来的批次号会直接害掉那批货，而一句「没查成」只会多走一趟台账。
func resolveBatches(ctx context.Context, pointID string, at time.Time, deps ChainDeps) ([]domain.Batch, *domain.BatchLookup) {
	if len(deps.Batches) > 0 {
		return deps.Batches, nil
	}
	if deps.BatchReader == nil {
		return nil, nil
	}
	lookup, err := deps.BatchReader.Batches(ctx, pointID, at)
	if err != nil {
		// 读侧实现可能只回错误、不回查询事实；这里把事实补齐，
		// 保证成稿拿到的永远是「查成了没有」这一事实，而不是一个空壳。
		if lookup.Status == "" {
			lookup.Status = domain.BatchLookupUnavailable
		}
		lookup.NeedsManual = true
		if strings.TrimSpace(lookup.Detail) == "" {
			lookup.Detail = errcode.As(err).Message
		}
	}
	if lookup.Source == "" {
		lookup.Source = domain.BatchSourceLedger
	}
	if lookup.CheckedAt.IsZero() {
		lookup.CheckedAt = at
	}
	return lookup.Batches, &lookup
}

// awaitConfirmation 标记判定已就绪：结论出了，停在药学部主管确认之前。
//
// 这一步不推进到「已处置」，也不触发出站写回：
// 批次到底能不能用，由疫苗生产企业评估质量影响、再由药学部主管确认。
func awaitConfirmation(ctx context.Context, in StepInput, now func() time.Time) (any, error) {
	recordValue, ok := in.Product(domain.PayloadExceptionRecord)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "待确认步骤缺少成稿产物")
	}
	record, ok := recordValue.(*domain.ExceptionRecordPayload)
	if !ok {
		return nil, errcode.Newf(errcode.Validation, "成稿产物类型不符：%T", recordValue)
	}
	if now == nil {
		now = time.Now
	}
	return &domain.ConfirmationReadyPayload{
		RunID:             in.Run.RunID,
		PointID:           in.Run.PointID,
		ExternalRef:       in.Run.ExternalRef,
		IdempotencyKey:    in.Run.IdempotencyKey,
		AwaitingRole:      "supervisor",
		OutstandingFields: record.MissingFields,
		ReadyAt:           now(),
	}, nil
}

// sampleRange 取一批采样的最早与最晚时刻。
func sampleRange(samples []domain.TemperatureEvent) (time.Time, time.Time) {
	from, to := samples[0].MeasuredAt, samples[0].MeasuredAt
	for _, sample := range samples[1:] {
		if sample.MeasuredAt.Before(from) {
			from = sample.MeasuredAt
		}
		if sample.MeasuredAt.After(to) {
			to = sample.MeasuredAt
		}
	}
	return from, to
}

// deviceNameOf 取采样里的设备名；没有就留空，不拿点位标识顶替设备名。
func deviceNameOf(samples []domain.TemperatureEvent) string {
	for _, sample := range samples {
		if sample.DeviceName != "" {
			return sample.DeviceName
		}
	}
	return ""
}
