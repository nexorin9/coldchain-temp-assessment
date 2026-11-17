package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
)

// 本文件是记录仪导出 CSV 的批量导入端点：POST /v1/imports/temperature-series。
//
// 这一段是现场补数据的入口。冷链记录仪大多把历史数据留在本地导出文件里，
// 断网期间的采样也只在这个文件里；设备恢复联网前，那些越温时段根本没人知道。
// 没有这条入口，这批数据就只能靠人工抄进 Excel，再由人工核对一遍——
// 而人工抄录的时间戳一改，越温窗口就跟着错。
//
// 与逐条事件端点的分工：逐条端点面向设备与网关的实时投递（一次一条、幂等键
// 逐条判）；本端点面向记录仪导出的历史文件（表头驱动、逐行给跳过原因、单事务落库）。
// 两条路径落的是同一张 events 表、同一套幂等键，所以重复导入同一份文件不会
// 产生第二份数据。
//
// 三条边界：
//   - 脏行不静默丢弃：每一行被跳过都带行号与可读原因回到调用方；
//   - 干跑只差落库：dry_run=true 的跳过统计与导入条数与实跑一致；
//   - 不越界下结论：导入只把采样稳稳落库，越温窗口与处置结论由判定链接手。

// defaultMaxImportBodyBytes 是批量导入请求体上限（16 MiB）。
//
// 比逐条事件的 1 MiB 宽：一份三个月的 5 分钟粒度导出约十万行、十几兆字节，
// 按 1 MiB 卡会把正常的历史补数据挡在门外。上限本身仍要留着——
// 院内小主机的内存余量并不宽裕。
const defaultMaxImportBodyBytes int64 = 16 << 20

// importResponse 是批量导入的回执。
//
// 回执要回答投递方四个问题：导进去多少条、跳过多少条、跳过的是哪几行
// 什么原因、库里原来已有的重复是多少条。四项都给全，调用方才不用
// 再回头查库核对一遍。
type importResponse struct {
	// Imported 是新写入的条数；干跑时是「将会写入」的条数。
	Imported int `json:"imported"`
	// Skipped 是因数据问题被跳过的条数。
	Skipped int `json:"skipped"`
	// TotalRows 是文件里的数据行总数（不含表头）。
	TotalRows int `json:"total_rows"`
	// Duplicates 是命中幂等键、库里已有的条数。
	Duplicates int `json:"duplicates"`
	// DryRun 标记本次是否只校验未落库。
	DryRun bool `json:"dry_run"`
	// SkipReasons 是逐条跳过原因。
	SkipReasons []ingest.SkipReason `json:"skip_reasons"`
	// Encoding 是这批文件的字符编码：utf-8 或 gbk。
	Encoding string `json:"encoding"`
	// EncodingFallback 表示原始字节不是合法 UTF-8、已回退 GBK 解析。
	EncodingFallback bool `json:"encoding_fallback"`
	// OriginalBytes 是导入文件的原始字节数。
	OriginalBytes int `json:"original_bytes"`
	// ExtraColumns 是表头里多出来的列名（厂家自有列，已忽略）。
	ExtraColumns []string `json:"extra_columns,omitempty"`
	// ExtraValues 是各行多出来的取值个数合计（已忽略）。
	ExtraValues int `json:"extra_values"`
	// MissingColumns 是表头里缺失的可选列名（已按缺测处理）。
	MissingColumns []string `json:"missing_columns,omitempty"`
	// Note 是给操作者看的一句话说明。
	Note string `json:"note"`
}

// handleImportSeries 受理一次记录仪导出 CSV 的批量导入。
//
// 顺序与逐条端点一致：先解码、再解析校验、后落库。
// 校验不过的行不进库，但会带着行号与原因出现在回执里。
func (s *Server) handleImportSeries(w http.ResponseWriter, r *http.Request) {
	if s.importer == nil {
		// 服务起得来但导入没装好：这是部署问题，如实报出来，
		// 不能装作导入过。
		writeError(w, errcode.Newf(errcode.StorageError,
			"批量导入未初始化，服务不接受导入请求"))
		return
	}

	dryRun, err := parseDryRun(r)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}

	body, err := s.readImportBody(w, r)
	if err != nil {
		writeError(w, errcode.As(err))
		return
	}

	result, err := s.importer.ImportSeries(r.Context(), body, dryRun)
	if err != nil {
		e := errcode.As(err)
		s.logWarn("import_failed", e, "")
		writeError(w, e)
		return
	}

	note := "导入完成：合法行已单事务入库；跳过的行见 skip_reasons"
	if dryRun {
		note = "干跑完成：只校验未落库，跳过统计与导入条数与实跑一致"
	}
	if result.EncodingFallback {
		note = "导入完成：原始文件不是合法 UTF-8，已回退 GBK 解析（encoding_fallback=true）"
	}

	writeJSON(w, http.StatusOK, importResponse{
		Imported:         result.Imported,
		Skipped:          result.Skipped,
		TotalRows:        result.TotalRows,
		Duplicates:       result.Duplicates,
		DryRun:           result.DryRun,
		SkipReasons:      result.SkipReasons,
		Encoding:         result.Encoding,
		EncodingFallback: result.EncodingFallback,
		OriginalBytes:    result.OriginalBytes,
		ExtraColumns:     result.ExtraColumns,
		ExtraValues:      result.ExtraValues,
		MissingColumns:   result.MissingColumns,
		Note:             note,
	})
}

// readImportBody 读入 CSV 请求体。
//
// 请求体过大 → E_VALIDATION（改小再传），请求体为空 → E_VALIDATION。
// 都不静默吞掉：操作者要知道自己传的是不是空文件。
func (s *Server) readImportBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, s.maxImportBodyBytes)
	defer func() { _ = limited.Close() }()

	data, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if ok := asMaxBytesError(err, &maxErr); ok {
			return nil, errcode.Newf(errcode.Validation,
				"导入请求体超过上限 %d 字节", s.maxImportBodyBytes)
		}
		return nil, errcode.Newf(errcode.Validation, "读取导入请求体失败：%v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, errcode.Newf(errcode.Validation, "导入请求体为空")
	}
	return data, nil
}

// parseDryRun 解析 dry_run 查询参数。
//
// 取值只认 1/true/yes 与 0/false/no（大小写不敏感）；给别的值直接报错，
// 不当成 false 悄悄跑一次实导入——把校验跑当成真导入，比导入本身更糟。
func parseDryRun(r *http.Request) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("dry_run"))
	if raw == "" {
		return false, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, errcode.Newf(errcode.Validation,
			"dry_run 取值不合法：%q（合法取值：true、false）", raw)
	}
}

// asMaxBytesError 把错误断言成 *http.MaxBytesError。
func asMaxBytesError(err error, target **http.MaxBytesError) bool {
	return errors.As(err, target)
}
