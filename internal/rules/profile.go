// 本文件落「点位温区配置能不能生效」这道闸门。
//
// 它和 zone.go 的关系：zone.go 回答「按这份配置该怎么判」，
// 本文件回答「这份配置能不能被登记下来」。两件事分开，是因为
// 「配置不合法」有两种截然不同的后果：
//
//   - 判的时候才发现不合法 → 这条判定转人工，人去查为什么；
//   - 登记的时候就不让写进去 → 配置根本没进库，下一次判定根本不会用到它。
//
// 后一种更靠前：配置写错了却已经进了登记簿，下一次判定就会拿一份错温区
// 算出「一切正常」的结论——那比转人工危险得多。
//
// 两条硬规则：
//   - 温区上下限必须合法：zone_min_c 不得大于 zone_max_c，且不得超出记录仪量程。
//   - 判定门槛必须显式填：min_excursion_minutes 与 sample_interval_sec 都得是正数。
//
// 第二条是本文件与 zone.go 唯一不同的地方，理由只有一条：
// 「不猜、不顶替」。门槛留空意味着「不知道连续超限多久才算越温」，
// 而系统一旦替现场填上一个默认值（哪怕是 20 分钟这个看着合理的数），
// 结论就成了系统的猜测。宁可让它显式填错被人看见，也不要它悄悄替人做主。
package rules

import (
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// ValidatePointProfile 校验一份点位温区配置能否登记生效。
//
// 校验口径与判定时完全一致（复用 ResolveZone 同一套规则），
// 避免出现「写得进去、判不出来」的配置——那种配置登记下来只是给下一班人挖坑。
//
// 返回的错误一律是 E_VALIDATION：调用方据此回 400，
// 并且明确不落库（配置没写进去，库里仍是原来那份可用的配置）。
func ValidatePointProfile(profile domain.PointProfile) error {
	if _, err := ResolveZone(&profile); err != nil {
		return err
	}
	if profile.MinExcursionMinutes <= 0 {
		return errcode.Newf(errcode.Validation,
			"点位 %s 的 min_excursion_minutes 必须为正数（当前 %d）："+
				"连续超限多久才算越温必须显式填写，系统不替现场填默认值",
			profile.PointID, profile.MinExcursionMinutes)
	}
	if profile.SampleIntervalSec <= 0 {
		return errcode.Newf(errcode.Validation,
			"点位 %s 的 sample_interval_sec 必须为正数（当前 %d）："+
				"采样周期是越限时长的折算口径，必须显式填写，系统不替现场填默认值",
			profile.PointID, profile.SampleIntervalSec)
	}
	return nil
}
