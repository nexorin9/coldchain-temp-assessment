// 本文件装配「模型归因 + 降级」的归因步骤处理器。
//
// 它是判定步骤链里归因那一步的在线实现：先问模型，模型给不出可用结论时
// 退回规则基线（internal/model/degrade.go），两条路落到同一个产物分码上。
//
// 为什么把降级放在处理器内部，而不是让调用方在外面判：
//
//	放在外面，调用方就得自己分「这次失败该不该降级」——而这个判断依据
//	（哪几类错误码意味着模型结论不可用）只有模型层知道。放在外面，
//	每个调用方都要抄一遍那张错误码名单，抄漏一处就多一条「模型挂了就整条
//	判定失败」的路径。
//
// 三条不许让步的约定：
//
//  1. **降级不推进业务状态**：降级产出的是一份带 degraded 标记的归因结论，
//     run 停在 awaiting_confirmation，不自动进 confirmed，不触发出站写回。
//  2. **只有可降级的错误才降**：校验类、存储类错误照常让这一步失败——
//     那是系统自己的问题，不该说成「模型不可用」。
//  3. **降级要留痕**：降级事件记进模型调用审计链（outcome=degraded），
//     复核的人能看出这条结论为什么退回来的。
package runtime

import (
	"context"
	"fmt"

	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/model"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
)

// ModelAttributionDeps 是模型归因处理器所需的依赖。
type ModelAttributionDeps struct {
	// Profile 是点位温区配置。缺它整条链装不起来：系统不猜温区、
	// 不用默认值顶替，宁可这条判定转人工。
	Profile *domain.PointProfile
	// Asker 是模型调用与事实校验的出口；为 nil 时装配即报错。
	Asker *model.Asker
	// Load 可选：在库装载情况（超量存放这条线索的判据）。
	//
	// 为 nil 时降级到规则基线仍能给出结论，只是少了「超量存放」这一个候选。
	Load *rules.LoadInfo
}

// NewModelAttributionHandler 装配「模型归因 + 降级」的归因步骤处理器。
//
// 返回的处理器把模型结论与降级结论落在同一个产物分码（attribution）上：
// 下游取产物时不必分「这次是模型给的还是规则给的」，要分的话看产物里的
// degraded 标记与降级原因即可。
func NewModelAttributionHandler(deps ModelAttributionDeps) (StepHandler, error) {
	if deps.Profile == nil {
		return nil, errcode.Newf(errcode.Validation,
			"缺少点位温区配置，模型归因处理器无法装配（系统不猜温区、不用默认值顶替）")
	}
	if deps.Asker == nil {
		return nil, errcode.Newf(errcode.Validation, "模型归因处理器缺少模型调用出口")
	}

	return func(ctx context.Context, in StepInput) (any, error) {
		series, window, err := attributionInputs(in)
		if err != nil {
			return nil, err
		}
		zone, err := rules.ResolveZoneForEvent(deps.Profile, series.Samples[0], nil)
		if err != nil {
			return nil, err
		}

		// 事实校验的依据与送给模型的输入同源：同一批采样、同一个窗口。
		// 两者不同源就会出现「模型看的是 A、校验用的是 B」这种各说各话。
		basis := model.FactBasis{
			PointID: in.Run.PointID,
			Samples: series.Samples,
			Window:  window,
		}
		// 这次调用属于哪条判定、哪一步，要带进上下文：
		// 审计链与校验记录都按它归位。
		ctx = audit.WithCallContext(ctx, in.Run.RunID, in.StepName)
		asker := deps.Asker.ForRun(in.Run.RunID, in.StepName, basis)

		parsed, _, err := asker.Ask(ctx, model.SchemaAttribution, basis.Payload())
		if err != nil {
			if !model.IsDegradable(err) {
				// 不是模型结论不可用，是这次判定本身出了问题：
				// 让这一步如实失败，不拿「模型不可用」当挡箭牌。
				return nil, err
			}
			payload, degradeErr := model.DegradeToBaseline(err, model.DegradeInput{
				Samples: series.Samples,
				Window:  window,
				Zone:    zone,
				Load:    deps.Load,
			})
			if degradeErr != nil {
				// 降级本身也失败：这不是「模型不可用」，是这条判定给不出结论。
				return nil, degradeErr
			}
			asker.RecordDegrade(ctx, model.SchemaAttribution, err)
			return payload, nil
		}

		attribution, ok := parsed.(*model.Attribution)
		if !ok {
			return nil, errcode.Newf(errcode.Validation,
				"模型归因结果类型不符：%T（分码 %q）", parsed, model.SchemaAttribution)
		}
		return model.AttributionPayloadFromModel(attribution), nil
	}, nil
}

// attributionInputs 从前序产物里取回归因需要的采样与越温窗口。
//
// 取不到就是取不到，不拿别的顶替：没有窗口就没有「这次越温持续了多久」，
// 归因里的时长判据会失真。
func attributionInputs(in StepInput) (*domain.SeriesPayload, *domain.ExcursionWindow, error) {
	seriesValue, ok := in.Product(domain.PayloadSeries)
	if !ok {
		return nil, nil, errcode.Newf(errcode.Validation, "归因步骤缺少取数产物")
	}
	series, ok := seriesValue.(*domain.SeriesPayload)
	if !ok {
		return nil, nil, errcode.Newf(errcode.Validation, "取数产物类型不符：%T", seriesValue)
	}
	if len(series.Samples) == 0 {
		return nil, nil, errcode.Newf(errcode.Validation, "归因步骤的取数产物里没有采样")
	}

	windowValue, ok := in.Product(domain.PayloadWindow)
	if !ok {
		return nil, nil, errcode.Newf(errcode.Validation, "归因步骤缺少越温窗口产物")
	}
	windowPayload, ok := windowValue.(*domain.WindowPayload)
	if !ok {
		return nil, nil, errcode.Newf(errcode.Validation, "越温窗口产物类型不符：%T", windowValue)
	}
	window := windowPayload.Window
	return series, &window, nil
}

// DegradeSummary 拼出一句人话的降级摘要，给日志与运行摘要用。
//
// 它回答的是「刚才那条判定为什么退回来了」——服务日志里有一条这个，
// 运维不用翻代码就知道端点那边出了什么事。
func DegradeSummary(run *domain.Run, attribution *domain.AttributionPayload) string {
	if run == nil || attribution == nil || !attribution.Degraded {
		return ""
	}
	reason := attribution.DegradeReason
	if reason == "" {
		reason = "模型归因不可用"
	}
	return fmt.Sprintf("run %s 归因降级：%s，结论由规则基线给出，停在待药学部主管确认，未触发出站写回",
		run.RunID, reason)
}
