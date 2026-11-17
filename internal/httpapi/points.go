// 本文件落两段「查」与一段「改」：判定台账查询、点位温区配置查询、点位温区配置更新。
//
// 三件事的背景是一样的——现场问的问题从来不是「系统现在怎么样」，
// 而是三句具体的问话：
//
//	「这周这个点位判过几段越温？」（判定台账查询）
//	「这台冷柜现在按什么温区判？」（温区配置查询）
//	「换了一批制品，把温区改成 2~8℃。」（温区配置更新）
//
// 温区配置更新是三者里唯一会改业务状态的一个，所以它的规矩最多：
//   - 写进去的是整份配置，不做局部合并——库里不留半新半旧的拼装结果；
//   - 校验不过就不落库：库里仍是原来那份可用的配置，不留一份写坏的；
//   - 只有经办与主管能改：改的是判定口径，不是数据，调阅岗不能改。
//
// 判定台账查询与温区配置查询都只读：调阅岗（质控 / 院感）要看的就是它们，
// 权限与「查看判定结论」同一把钥匙。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
)

// RunLister 是判定 run 台账查询的能力边界。
//
// 与 RunReader 分开声明：RunReader 取的是「一条 run 的完整细节」，
// RunLister 取的是「一段时间内判过哪些 run」——两条查询口径不同，
// 放在一起会让「查清单」这件事被迫依赖「查详情」那套取回逻辑。
type RunLister interface {
	// RunsByPointInRange 返回某点位在时间区间内的判定 run，按窗口起点升序。
	// pointID 为空表示不限点位；from / to 为零值表示该端不限。
	RunsByPointInRange(ctx context.Context, pointID string, from, to time.Time) ([]domain.RunRecord, error)
}

// PointProfileRegistry 是点位温区配置的读写能力边界。
//
// 只声明本包真正用到的方法。校验不在这里：配置的合法性由 rules 判定，
// 这一层只负责「把请求变成配置、把配置变成回执」。
type PointProfileRegistry interface {
	// UpsertPointProfile 写入或覆盖一个点位的温区配置。
	UpsertPointProfile(ctx context.Context, profile domain.PointProfile) error
	// PointProfileByID 回读一个点位的温区配置；未登记时返回 E_NOT_FOUND。
	PointProfileByID(ctx context.Context, pointID string) (domain.PointProfile, error)
	// PointProfiles 回读全部已登记的点位温区配置，按 point_id 升序。
	PointProfiles(ctx context.Context) ([]domain.PointProfile, error)
}

// pointProfileListResponse 是 GET /v1/points 的响应体。
type pointProfileListResponse struct {
	// Count 是已登记的点位数。
	Count int `json:"count"`
	// Profiles 是各点位的温区配置，按 point_id 升序。
	Profiles []domain.PointProfile `json:"profiles"`
	// Note 是给现场的一句话说明。
	Note string `json:"note"`
}

// pointProfileResponse 是 PUT /v1/points/{point_id}/profile 的响应体。
type pointProfileResponse struct {
	// PointID 是这份配置适用的点位。
	PointID string `json:"point_id"`
	// Profile 是登记生效的整份配置。
	Profile domain.PointProfile `json:"profile"`
	// UpdatedAt 是配置写入时刻（RFC3339）。
	UpdatedAt string `json:"updated_at"`
	// Note 说明这次写入的影响范围。
	Note string `json:"note"`
}

// runListResponse 是 GET /v1/runs 的响应体。
type runListResponse struct {
	// Count 是本次返回的判定条数。
	Count int `json:"count"`
	// PointID 是本次查询限定的点位；未限定时为空。
	PointID string `json:"point_id,omitempty"`
	// From / To 是本次查询的区间（RFC3339）；未限定的一端为空。
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Runs 是判定清单，按窗口起点升序。
	Runs []runView `json:"runs"`
	// Note 是给现场的一句话说明。
	Note string `json:"note"`
}

// handleListRuns 返回判定台账清单。
//
// 三个查询参数都是可选的：point_id 限点位，from / to 限窗口起点区间。
// 区间按窗口起点取，不含端点为闭区间——现场按「这周的越温」查，
// 习惯上希望把这一周的边界时刻都算进来。
//
// 参数格式错一律回 400 并说清格式，不做「尽力猜一个」：日期被悄悄解释成
// 某一天的零点或某个时区，得到的清单就会少算或多算一整段越温，
// 而清单看起来「正常」，没人会去核。
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if s.runLister == nil {
		writeError(w, errcode.Newf(errcode.NotFound, "本服务未装配判定台账查询能力"))
		return
	}

	query := r.URL.Query()
	pointID := strings.TrimSpace(query.Get("point_id"))

	from, err := parseRangeTime(query.Get("from"), "from")
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}
	to, err := parseRangeTime(query.Get("to"), "to")
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		writeError(w, errcode.Newf(errcode.Validation,
			"查询区间非法：to=%s 早于 from=%s", to.Format(time.RFC3339), from.Format(time.RFC3339)))
		return
	}

	records, err := s.runLister.RunsByPointInRange(r.Context(), pointID, from, to)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}

	views := make([]runView, 0, len(records))
	for _, rec := range records {
		views = append(views, buildRunView(rec))
	}
	writeJSON(w, http.StatusOK, runListResponse{
		Count:   len(views),
		PointID: pointID,
		From:    formatTime(from),
		To:      formatTime(to),
		Runs:    views,
		Note:    "清单按窗口起点升序；窗口起点落在查询区间内的判定才会列出（未设窗口的采样不计入）",
	})
}

// parseRangeTime 解析查询区间的一个端点。
//
// 只接受 RFC3339 带偏移的时刻。日期（2026-10-20）不接受：
// 它没有时区，也没有「到当天几点」的意思，替它猜一个等于替现场做决定。
func parseRangeTime(raw, name string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, errcode.Newf(errcode.Validation,
			"%s 必须是 RFC3339 时刻（如 2026-10-20T00:00:00+08:00），当前值 %q 无法解析",
			name, trimmed)
	}
	return parsed, nil
}

// handleListPointProfiles 返回已登记的点位温区配置清单。
//
// 它回答的是「这台冷柜现在按什么温区判」：判定结论里的 2~8℃ 从哪来，
// 复核的人第一句要问的就是这个。
func (s *Server) handleListPointProfiles(w http.ResponseWriter, r *http.Request) {
	if s.pointProfiles == nil {
		writeError(w, errcode.Newf(errcode.NotFound, "本服务未装配点位温区配置查询能力"))
		return
	}
	profiles, err := s.pointProfiles.PointProfiles(r.Context())
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}
	writeJSON(w, http.StatusOK, pointProfileListResponse{
		Count:    len(profiles),
		Profiles: profiles,
		Note:     "清单按 point_id 升序；已判定的历史结论仍按判定当时的配置快照复算，不受这里的更新影响",
	})
}

// handleUpdatePointProfile 登记或更新一个点位的温区配置。
//
// 校验不过就不落库：库里那份配置原样留着，继续可用。
// 这条很重要——配置写坏了却已经进了登记簿，下一次判定就会拿一份错温区
// 算出「一切正常」的结论。
//
// 路径里的 point_id 是这份配置的归属；请求体里若也带了 point_id，
// 两处必须一致，不一致直接拒绝：悄悄挑一个会让这份配置挂到错误的冷柜上。
func (s *Server) handleUpdatePointProfile(w http.ResponseWriter, r *http.Request) {
	if s.pointProfiles == nil {
		writeError(w, errcode.Newf(errcode.NotFound, "本服务未装配点位温区配置维护能力"))
		return
	}

	pathPointID := strings.TrimSpace(r.PathValue("point_id"))
	if pathPointID == "" {
		writeError(w, errcode.Newf(errcode.Validation, "缺少 point_id"))
		return
	}

	body, err := s.decodePointProfile(w, r)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}
	profile := body

	if bodyPointID := strings.TrimSpace(profile.PointID); bodyPointID != "" && bodyPointID != pathPointID {
		writeError(w, errcode.Newf(errcode.Validation,
			"路径里的 point_id（%s）与请求体里的 point_id（%s）不一致，拒绝登记",
			pathPointID, bodyPointID))
		return
	}
	profile.PointID = pathPointID

	if err := rules.ValidatePointProfile(profile); err != nil {
		writeError(w, errcode.As(err))
		return
	}

	if err := s.pointProfiles.UpsertPointProfile(r.Context(), profile); err != nil {
		writeError(w, errcode.As(err))
		return
	}

	writeJSON(w, http.StatusOK, pointProfileResponse{
		PointID:   profile.PointID,
		Profile:   profile,
		UpdatedAt: time.Now().Format(time.RFC3339),
		Note:      "配置已登记生效；之后新建的判定按这份配置判，已判定的历史结论仍按当时的配置快照复算",
	})
}

// decodePointProfile 读入并解析点位温区配置的请求体。
//
// 三类失败各有各的说法：请求体过大 → E_VALIDATION（改小再投），
// 请求体不是合法 JSON → E_VALIDATION，尾部多出内容 → E_VALIDATION。
// 空体也拒：配置是整份写入的，空体意味着「什么都没写」，
// 却会把库里的配置清成一份空壳。
func (s *Server) decodePointProfile(w http.ResponseWriter, r *http.Request) (domain.PointProfile, error) {
	limited := http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	defer func() { _ = limited.Close() }()

	decoder := json.NewDecoder(limited)
	var profile domain.PointProfile
	if err := decoder.Decode(&profile); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return domain.PointProfile{}, errcode.Newf(errcode.Validation,
				"请求体超过上限 %d 字节", s.maxBodyBytes)
		case errors.Is(err, io.EOF):
			return domain.PointProfile{}, errcode.Newf(errcode.Validation,
				"请求体为空：点位温区配置是整份写入的，不能只提交一部分")
		default:
			return domain.PointProfile{}, errcode.Newf(errcode.Validation,
				"请求体不是合法的点位温区配置 JSON：%v", err)
		}
	}

	// 一次只允许一份配置对象：多出来的内容要么是调用方拼错了，
	// 要么是想把两份配置塞进一次提交。两种都得说出来。
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.PointProfile{}, errcode.Newf(errcode.Validation,
			"请求体只能包含一个点位温区配置 JSON 对象")
	}

	return profile, nil
}
