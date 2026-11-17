package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
)

// 本文件是入站温度事件端点：POST /v1/events/coldchain-temperature。
//
// 这一段只回答「收没收到、收到的是哪一条」，不回答「这段温度意味着什么」：
// 越温窗口、原因判断与异常记录表由判定链接手，事件接入不越界下结论。
//
// 与判定链的关系要讲清楚：判定 run 由调度器按点位与窗口建立，
// 同一越温窗口只判一次；本端点此刻只负责把采样稳稳落库并给出回执。

// defaultMaxBodyBytes 是入站请求体上限（1 MiB）。
//
// 一条温度事件只有几百字节，1 MiB 足够宽裕；设上限是为了防止
// 误配置或恶意的超大请求把内存吃光——院内小主机的余量并不宽裕。
const defaultMaxBodyBytes int64 = 1 << 20

// runStatePending 表示事件已入库、判定 run 尚未建立。
//
// 调度器（run 调度任务）落地后，这里会改成 run 建立后的真实状态。
const runStatePending = "pending"

// eventAcceptedResponse 是入站事件受理成功的回执。
//
// 回执要回答投递方三个问题：收到了没有、这条采样在库里的标识是什么、
// 它归到哪条判定上。前两个现在就有答案；第三个（run_id）要等判定 run
// 建立后才填得上，所以还没建立时这一项直接不出现（而不是给一个 null
// 或者先编一个号出来）——「没有」和「有但为空」在接口上不是一回事，
// 调用方按字段在不在来判断，不用每次都先判一次 null。
type eventAcceptedResponse struct {
	// EventID 是该采样在库里的标识；重复投递时与首次一致。
	EventID string `json:"event_id"`
	// Duplicate 表示本次投递命中了已有的同一条采样，没有新增记录。
	Duplicate bool `json:"duplicate"`
	// PointID 是温控点位标识，回执带回去，投递方不必自己再记一遍。
	PointID string `json:"point_id"`
	// MeasuredAt 是采样时刻，保留记录仪上报时的时区偏移。
	MeasuredAt string `json:"measured_at"`
	// AcceptedAt 是本服务受理该投递的时刻（UTC）。
	AcceptedAt string `json:"accepted_at"`
	// RunID 是判定 run 的标识；run 尚未建立时这一项不出现。
	RunID *string `json:"run_id,omitempty"`
	// RunState 说明这条事件当前在判定链上的位置。
	RunState string `json:"run_state"`
	// Note 是给投递方看的一句话：同窗去抖、重复投递等情况都写在这里。
	Note string `json:"note"`
	// Precision 是测温精度标注。精度不足时投递方当场就能看到「仅供参考」，
	// 不必等到看结论时才发现这条读数本来就支撑不了量化结论。
	Precision domain.PrecisionNote `json:"precision"`
}

// handleIngestEvent 受理一条入站温度事件。
//
// 顺序是刻意的：先解码、再校验、后落库。校验不过的数据不进库——
// 库里的每一条采样都必须是「设备当时真的报过的」那一条。
func (s *Server) handleIngestEvent(w http.ResponseWriter, r *http.Request) {
	if s.ingestor == nil {
		// 服务起得来但接入没装好：这是部署问题，如实报出来，
		// 不能装作收到了。
		writeError(w, errcode.Newf(errcode.StorageError,
			"事件接入未初始化，服务不接受事件投递"))
		return
	}

	raw, err := s.decodeEvent(w, r)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}

	event, err := ingest.NormalizeEvent(raw)
	if err != nil {
		e := errcode.As(err)
		s.logWarn("ingest_rejected", e, event.PointID)
		writeError(w, e)
		return
	}

	result, err := s.ingestor.Ingest(r.Context(), event)
	if err != nil {
		e := errcode.As(err)
		s.logWarn("ingest_failed", e, event.PointID)
		writeError(w, e)
		return
	}

	note := "事件已入库；判定 run 由调度器按点位与窗口建立，同一越温窗口只判一次"
	if result.Duplicate {
		note = "重复投递：命中幂等键，未新增记录"
	}

	writeJSON(w, http.StatusAccepted, eventAcceptedResponse{
		EventID:    result.EventID,
		Duplicate:  result.Duplicate,
		PointID:    result.PointID,
		MeasuredAt: result.MeasuredAt.Format(time.RFC3339),
		AcceptedAt: time.Now().UTC().Format(time.RFC3339),
		RunID:      nil,
		RunState:   runStatePending,
		Note:       note,
		Precision:  event.Precision,
	})
}

// decodeEvent 读入并解析请求体。
//
// 三类失败各有各的错误码：请求体过大 → E_VALIDATION（改小再投），
// 请求体不是合法 JSON → E_VALIDATION，尾部多出内容 → E_VALIDATION。
// 都不静默吞掉：投递方要知道自己错在哪，才能改对。
func (s *Server) decodeEvent(w http.ResponseWriter, r *http.Request) (ingest.RawEvent, error) {
	limited := http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	defer func() { _ = limited.Close() }()

	decoder := json.NewDecoder(limited)
	var raw ingest.RawEvent
	if err := decoder.Decode(&raw); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return ingest.RawEvent{}, errcode.Newf(errcode.Validation,
				"请求体超过上限 %d 字节", s.maxBodyBytes)
		case errors.Is(err, io.EOF):
			return ingest.RawEvent{}, errcode.Newf(errcode.Validation, "请求体为空")
		default:
			return ingest.RawEvent{}, errcode.Newf(errcode.Validation,
				"请求体不是合法的温度事件 JSON：%v", err)
		}
	}

	// 一次投递只允许一个 JSON 对象：多出来的内容要么是调用方拼错了，
	// 要么是想把两批数据塞进一次投递。两种都得说出来，不能默默只取第一个。
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ingest.RawEvent{}, errcode.Newf(errcode.Validation,
			"请求体只能包含一个温度事件 JSON 对象")
	}

	return raw, nil
}

// logWarn 记录一条带错误码的告警日志；没有配置日志器时静默跳过。
func (s *Server) logWarn(event string, e *errcode.Error, pointID string) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(event,
		"error_code", string(e.Code),
		"detail", e.Details,
		"point_id", pointID)
}
