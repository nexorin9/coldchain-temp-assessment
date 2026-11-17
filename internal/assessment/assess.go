// Package assessment 串起冷链判定的最窄主路径。
//
// 主路径只有三段：温度采样进来 → 建一条判定 run → 按温区比较抽出越限窗口，
// 产出一份可核对的结论 {run, window}。
//
// 这一段刻意不碰模型与出站写回：先把「输入 → 处理 → 可核对产出」跑通，
// 后续步骤是在同一条主路径上加深（归因、成稿、确认、写回），不是并列的第二套模式。
package assessment

import (
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// Result 是一次判定的结论。
//
// Run 是判定过程的容器；Window 是规则抽出的越温窗口（未越限时为 null）。
// Zone 是温区判定结论：温区是谁定的、依据哪一条、精度够不够、
// 设备归属核验到哪一步——这些必须跟着结论一起走，
// 否则事后只剩一个「2~8℃」的数字，没人说得清它从哪来。
// NeedsManualReason 只在系统判不了时出现，说明「为什么须人工接手」。
//
// Excursions 是这批采样里的越限汇总。窗口为 nil 时，它是唯一能说明
// 「到底越没越限」的地方：没有它，光看 window=null 分不出
// 「压根没越限」和「越了但没够最短时长门槛」——这两种情况对药师的含义完全不同。
//
// Windows 是这批采样里成立的**全部**正式窗口（相邻越限已合并）。
// Window 是其中第一条：一条判定 run 对应一个越温窗口，
// 但同一批采样里若有第二次独立异常，它同样要出现在结论里，不能被吞掉。
//
// TransientExcursions 是未达最短越限门槛的短时波动。
// 它们不生成正式窗口、也不进台账，但要单独列出来给药师看——
// 「没成立窗口」和「压根没越限」是两回事。
//
// Features 是这次越温的归因特征：温升速率、峰值偏离度、湿度上升幅度、
// 开门累计时长与滞后、断电与除霜、相邻采样跳变、设备精度声明。
// 它们是规则基线归因与模型归因共用的输入面——两边看同一批数字，
// 结论不一致时才能逐条对照。特征算不出来不影响窗口结论，
// 但原因要留在 FeaturesReason 里，不静默咽下去。
type Result struct {
	Run                 domain.Run                  `json:"run"`
	Window              *domain.ExcursionWindow     `json:"window"`
	Windows             []domain.ExcursionWindow    `json:"windows,omitempty"`
	Zone                *rules.ZoneDecision         `json:"zone,omitempty"`
	Excursions          *rules.ExcursionSummary     `json:"excursions,omitempty"`
	TransientExcursions []domain.TransientExcursion `json:"transient_excursions,omitempty"`
	Features            *domain.ExcursionFeatures   `json:"features,omitempty"`
	FeaturesReason      string                      `json:"features_reason,omitempty"`
	NeedsManualReason   string                      `json:"needs_manual_reason,omitempty"`
}

// Assess 对一组温度采样做最窄判定。
//
// 配置缺失、点位歧义等「系统判不了」的情况，返回一条 state 为 needs_manual 的 run
// 与 E_VALIDATION 错误：调用方能同时拿到「系统判不了」这个事实和「为什么」，
// 而不是拿到一个凑出来的温区结论。
func Assess(samples []domain.TemperatureEvent, profile *domain.PointProfile) (Result, error) {
	return AssessWithBatches(samples, profile, nil)
}

// AssessWithBatches 带着在库批次清单做判定。
//
// batches 来自冷链台账（批次号只由台账给）：给了它，判定才能把在库批次
// 挂到各自的制品温区下，并发现「在库有货却查不到温区」这种说不清的情况。
// 不给它（台账还没接上、或本次判定不涉批次），判定照常做，只是不做批次映射。
func AssessWithBatches(samples []domain.TemperatureEvent, profile *domain.PointProfile, batches []domain.Batch) (Result, error) {
	if len(samples) == 0 {
		return Result{}, errcode.Newf(errcode.Validation, "没有可判定的温度采样")
	}

	pointID := samples[0].PointID
	run := domain.Run{
		RunID:   domain.NewRunID(),
		PointID: pointID,
		State:   domain.RunStateDetected,
	}

	// 一段采样必须来自同一个点位：混了点位的采样无法拆成一条判定。
	for _, s := range samples[1:] {
		if s.PointID != pointID {
			return Result{
					Run:               run,
					NeedsManualReason: "同一批采样含多个点位，无法作为一次判定",
				}, failToManual(&run, "mixed_points", "同一批采样含多个点位，无法作为一次判定",
					errcode.Newf(errcode.Validation,
						"同一批采样含多个点位：%s 与 %s", pointID, s.PointID))
		}
	}

	// 温区闸门：配置缺失、点位对不上、设备归属歧义、批次挂不上温区，
	// 一律转人工，不给凑出来的结论。
	zone, err := rules.ResolveZoneForEvent(profile, samples[0], batches)
	if err != nil {
		return Result{Run: run, Zone: &zone, NeedsManualReason: zone.Reason},
			failToManual(&run, "zone_unusable", zone.Reason, err)
	}

	// 窗口与短时波动出自同一遍扫描：哪些算越温、哪些算波动，
	// 两边不会互相打架。未达门槛的越限单独归集，不生成正式窗口。
	windows, transients, err := rules.ExtractExcursionReport(samples, zone)
	if err != nil {
		// 温区判定不可用：不产出窗口结论，run 落 needs_manual。
		return Result{Run: run, Zone: &zone, NeedsManualReason: zone.Reason},
			failToManual(&run, "window_not_extractable", zone.Reason, err)
	}

	// 越限汇总与窗口用同一套扫描算出来，两边对「哪几段越限」的判断不会打架。
	// 窗口没成立时，它就是「为什么没成立」的答案。
	summary, err := rules.SummarizeExcursions(samples, zone)
	if err != nil {
		return Result{Run: run, Zone: &zone, NeedsManualReason: zone.Reason},
			failToManual(&run, "excursion_summary_failed", zone.Reason, err)
	}

	var window *domain.ExcursionWindow
	if len(windows) > 0 {
		window = &windows[0]
	}

	if window == nil {
		// 没有达标的越温窗口：run 已建，越限情况见 Excursions，短时波动见 TransientExcursions。
		return Result{
			Run:                 run,
			Window:              nil,
			Windows:             windows,
			Zone:                &zone,
			Excursions:          &summary,
			TransientExcursions: transients,
		}, nil
	}

	// 窗口起止、外部引用与幂等键同源同算（BindWindow 一处写齐）。
	run.BindWindow(window.Start, window.End)
	if _, err := run.AdvanceState(workflow.Request{
		To:     workflow.StateAssessed,
		Action: workflow.ActionAuto,
		Reason: "excursion_window_extracted",
		Note:   "越温窗口已抽出，给出可核对的判定结论",
		Actor:  workflow.ActorSystem,
	}); err != nil {
		// 状态推进被拒（迁移不合法）：保留原状态并把原因带回，不硬改。
		return Result{Run: run, Zone: &zone, NeedsManualReason: zone.Reason}, err
	}

	// 归因特征：窗口连同周边观测算出的可核对特征面。
	// 特征算不出来不推翻窗口结论——窗口是按温区算的，特征只是归因的输入面；
	// 但原因要留在 FeaturesReason 里，不静默咽下去。
	features, featuresReason := extractFeatures(samples, window, zone)

	return Result{
		Run:                 run,
		Window:              window,
		Windows:             windows,
		Zone:                &zone,
		Excursions:          &summary,
		TransientExcursions: transients,
		Features:            features,
		FeaturesReason:      featuresReason,
	}, nil
}

// failToManual 把一条判不动的 run 落成「需人工处理」，并把判定本身的错误带回。
//
// 状态推进走 AdvanceState 一处（唯一入口）：迁移表、终态保护与迁移留痕都在那儿。
// 推进被拒时不硬改状态——那种情况下保留原状态并把原因一并带回去，
// 比悄悄把状态改成 needs_manual 让人以为「系统已经判过了」要安全。
//
// 返回的错误是判定本身的错误（调用方要看到「为什么判不了」），
// 只有在状态推进被拒时才附上推进失败的原因。
func failToManual(run *domain.Run, reason, note string, cause error) error {
	if _, err := run.AdvanceState(workflow.Request{
		To:     workflow.StateNeedsManual,
		Action: workflow.ActionAuto,
		Reason: reason,
		Note:   note,
		Actor:  workflow.ActorSystem,
	}); err != nil {
		return errcode.Newf(errcode.IllegalTransition,
			"判定转人工时状态推进被拒：%v（原始原因：%v）", err, cause)
	}
	return cause
}

// extractFeatures 提取归因特征，并把失败原因如实带回。
func extractFeatures(samples []domain.TemperatureEvent, window *domain.ExcursionWindow, zone rules.ZoneDecision) (*domain.ExcursionFeatures, string) {
	f, err := rules.ExtractFeatures(samples, window, zone)
	if err != nil {
		return nil, err.Error()
	}
	return &f, ""
}
