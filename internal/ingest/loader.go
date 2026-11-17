package ingest

import (
	"bytes"
	"encoding/json"
	"os"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// LoadEventFile 从 JSON 文件读入一条入站温度事件（线格式）。
//
// 这里只负责「读进来」，字段是否合法由 Validate 判定，
// 两类失败分别给明确错误码：文件不存在 → E_NOT_FOUND，文件不是合法 JSON → E_VALIDATION。
func LoadEventFile(path string) (RawEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RawEvent{}, errcode.Newf(errcode.NotFound, "事件文件不存在：%s", path)
		}
		return RawEvent{}, errcode.Newf(errcode.Validation, "读取事件文件失败：%s", err)
	}
	var raw RawEvent
	if err := json.Unmarshal(data, &raw); err != nil {
		return RawEvent{}, errcode.Newf(errcode.Validation, "事件文件不是合法 JSON：%s", err)
	}
	return raw, nil
}

// LoadEventSeries 从 JSON 文件读入一组温度事件（线格式）。
//
// 支持两种文件形态：JSON 数组（一段采样序列）或单个事件对象（单点样例）。
// 单点样例是合法的输入形态，不要求现场先凑出一段序列才能看结论。
//
// 错误语义与 LoadEventFile 一致：文件不存在 → E_NOT_FOUND，
// 文件不是合法 JSON / 数组为空 → E_VALIDATION。
func LoadEventSeries(path string) ([]RawEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errcode.Newf(errcode.NotFound, "事件文件不存在：%s", path)
		}
		return nil, errcode.Newf(errcode.Validation, "读取事件文件失败：%s", err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errcode.Newf(errcode.Validation, "事件文件为空：%s", path)
	}
	if trimmed[0] == '[' {
		var raws []RawEvent
		if err := json.Unmarshal(trimmed, &raws); err != nil {
			return nil, errcode.Newf(errcode.Validation, "事件文件不是合法的 JSON 数组：%s", err)
		}
		if len(raws) == 0 {
			return nil, errcode.Newf(errcode.Validation, "事件文件里没有任何温度事件：%s", path)
		}
		return raws, nil
	}
	var raw RawEvent
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, errcode.Newf(errcode.Validation, "事件文件不是合法 JSON：%s", err)
	}
	return []RawEvent{raw}, nil
}

// LoadProfileFile 从 JSON 文件读入点位温区配置。
//
// 温区配置是判定的权威输入，缺它就不判：读不出配置时返回 E_NOT_FOUND / E_VALIDATION，
// 由调用方把 run 落 needs_manual，绝不用默认温区顶替。
func LoadProfileFile(path string) (domain.PointProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.PointProfile{}, errcode.Newf(errcode.NotFound, "温区配置文件不存在：%s", path)
		}
		return domain.PointProfile{}, errcode.Newf(errcode.Validation, "读取温区配置文件失败：%s", err)
	}
	var profile domain.PointProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return domain.PointProfile{}, errcode.Newf(errcode.Validation, "温区配置文件不是合法 JSON：%s", err)
	}
	return profile, nil
}
