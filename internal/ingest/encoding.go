package ingest

import (
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件处理真实院内记录仪导出文件的「数据形态」问题——
// 也就是那些不影响数据正确性、但会让导入当场炸掉或悄悄变形的东西。
//
// 现场导出文件常见的五种脏法，每一种都在这里有明确落点：
//
//  1. 编码不是 UTF-8。Windows 上跑的记录仪软件导出 CSV，十有八九是 GBK。
//     直接按 UTF-8 解会得到一片乱码，设备名与点位名全废；
//     这里是先验 UTF-8，不合法就回退 GBK，并在事件上标 encoding_fallback。
//
//  2. 字段超长。厂家在某个字段里塞了一整段说明文字是常事，
//     不设上限的话，库表与日志会被单个字段撑爆。
//     这里按字段长度上限截断，并标 truncated_fields——截了要让人知道。
//
//  3. 全半角混排。中文 Windows 的输入法与部分仪器上位机，
//     会把温度写成全角「１２．３」。不转半角，ParseFloat 直接失败，
//     一整批数据被当成坏行跳过。
//
//  4. 可选列空着或整列缺失。缺测就是缺测，填 0 冒充是数据造假；
//     这里保持 nil，并把缺测的字段名记进 missing_fields。
//
//  5. 多余的列。不同型号设备导出的列不一样，多出来的列忽略即可，
//     但要计数并报出去——否则现场会以为「导进来 12 列全用上了」。
//
// 一条底线贯穿本文件：所有形态问题都显式标注，不静默改数据。
// 能修的（编码、全角、空白）修完要在事件上留痕，不能修的（截断、缺测、多余）
// 如实标注，让后面出结论的人知道这份数据的成色，而不是拿着一份
// 「看起来干净、其实已经被悄悄改过」的数据去下判断。

// 字符编码标注。
const (
	// EncodingUTF8 是标准 UTF-8 编码。
	EncodingUTF8 = "utf-8"
	// EncodingGBK 是简体中文 GBK 编码（Windows 中文版记录仪导出的常见形态）。
	EncodingGBK = "gbk"
)

// 字段长度上限（按字符数，不是字节数）。
//
// 按字符数截是为了不把一个汉字劈成半个——半个汉字进库就是乱码。
// 上限取值按「够放下正常值、又拦得住异常值」来定：
// 正常点位名与设备名十几个字足够；上百字符的字段基本是厂家塞了说明文字。
const (
	maxPointIDRunes    = 64
	maxDeviceIDRunes   = 64
	maxDeviceNameRunes = 128
	maxSourceRunes     = 32
	maxUnitRunes       = 16
	maxEventsRunes     = 256
)

// utf8BOM 是 UTF-8 字节序标记。
//
// Excel 导出的 CSV 常常带 BOM。不去掉的话，BOM 会粘在第一个列名前面，
// 表头校验就报「缺少必需列 point_id」——文件明明是对的，却导不进去。
const utf8BOM = "\xef\xbb\xbf"

// DecodedSeries 是一批导出字节的解码结果。
type DecodedSeries struct {
	// Text 是解码后的文本，可直接交给 CSV 解析器。
	Text string
	// Encoding 是判定的编码：utf-8 或 gbk。
	Encoding string
	// Fallback 表示原始字节不是合法 UTF-8、已回退 GBK 解析。
	Fallback bool
	// OriginalBytes 是原始字节数，回退发生时记进日志用。
	OriginalBytes int
}

// DecodeSeriesBytes 解码一段记录仪导出文件的字节流。
//
// 判定顺序：先验 UTF-8；不合法再试 GBK。
// GBK 解出来若含替换字符（U+FFFD），说明这批字节既不是 UTF-8 也不是 GBK
// （比如 GB18030 扩展区或 UTF-16），此时明确报错而不是吐一堆乱码进库——
// 乱码进库比导不进来更糟，它会让人误以为这批数据是好的。
//
// 回退发生时只在事件上标注，不改判定口径：温度数值、单位、时间解析
// 都按解码后的文本照常进行，和 UTF-8 文件走的是同一段代码。
func DecodeSeriesBytes(data []byte) (DecodedSeries, error) {
	out := DecodedSeries{OriginalBytes: len(data)}

	if utf8.Valid(data) {
		out.Text = strings.TrimPrefix(string(data), utf8BOM)
		out.Encoding = EncodingUTF8
		return out, nil
	}

	// GBK 解码器每次新建：解码器带内部状态，复用会让上一次解码的残留
	// 漏进下一次结果里（实测过一次：先解坏字节再解正常文本，正常文本也解错了）。
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(data)
	if err != nil {
		return DecodedSeries{}, errcode.Newf(errcode.Validation,
			"导入文件既不是合法 UTF-8 也不是合法 GBK：%s", err)
	}
	if strings.ContainsRune(string(decoded), utf8.RuneError) {
		return DecodedSeries{}, errcode.Newf(errcode.Validation,
			"导入文件既不是合法 UTF-8 也不是合法 GBK（含无法解码的字节，%d 字节）",
			len(data))
	}

	out.Text = strings.TrimPrefix(string(decoded), utf8BOM)
	out.Encoding = EncodingGBK
	out.Fallback = true
	return out, nil
}

// normalizeNumericCell 归一化数值单元格：去首尾空白 + 全角转半角。
//
// 全角数字在中文 Windows 上很常见（输入法默认状态、「．」代替小数点）。
// 不转的话 strconv.ParseFloat 直接报错，一整批数据被当成坏行跳过。
//
// 只动数值字段：字符串字段保持原文，全角字符在设备名里也是合法内容。
func normalizeNumericCell(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '０' && r <= '９': // 全角数字 ０-９
			return r - '０' + '0'
		case r == '．' || r == '。': // 全角句点（部分仪器用它当小数点）
			return '.'
		case r == '－' || r == '—': // 全角连字符
			return '-'
		case r == '＋':
			return '+'
		case r == '　': // 全角空格
			return ' '
		default:
			return r
		}
	}, trimmed)
}

// normalizeLatinCell 把全角 ASCII 字母与符号折成半角。
//
// 全角的「Ｃ」（U+FF23）与半角的 C 看着一样，但不相等：
// 单位校验只认半角时，「Ｃ」会被判成非法单位，整行数据被跳过。
//
// 这一层只用于「枚举字母」字段（单位、来源）：这类字段的取值空间是
// 固定几个词，全半角之争只可能是输入法手滑，折成半角不丢信息。
// 设备名、点位名这类标识字段一律不折，原文保留。
func normalizeLatinCell(raw string) string {
	trimmed := strings.TrimSpace(raw)
	return strings.Map(func(r rune) rune {
		if r >= '！' && r <= '～' { // 全角 ASCII 区 U+FF01–U+FF5E
			return r - '！' + '!'
		}
		return r
	}, trimmed)
}

// canonicalSource 把来源字段归一成规范取值。
//
// 来源是固定几个词的枚举（recorder / edge-gateway / manual-upload），
// 全角字母折成半角后才是同一个词；不折的话，精度上限会按错误的来源取，
// 把人工抄录的读数当成记录仪读数来判精度。
func canonicalSource(raw string) string {
	return normalizeLatinCell(raw)
}

// normalizeTextCell 归一化文本单元格：只去首尾空白，原文不改写。
//
// 文本字段不做全角转半角、不做繁简转换、不做任何「规范化」：
// 设备名里的全角括号、中文引号、空格都是现场标识的一部分，
// 改写了就追不回是哪台冰箱。
func normalizeTextCell(raw string) string {
	return strings.TrimSpace(raw)
}

// truncateCell 按字符数截断超长字段，返回截断后的文本与是否发生了截断。
//
// 按字符截而不是按字节切：按字节切会把一个汉字劈成两半，
// 半个汉字进库就是乱码，比截断本身更糟。
func truncateCell(raw string, max int) (string, bool) {
	if max <= 0 {
		return raw, false
	}
	runes := []rune(raw)
	if len(runes) <= max {
		return raw, false
	}
	return string(runes[:max]), true
}

// cellText 取文本字段：去首尾空白 + 超长截断，并把截断记进行级标注。
func cellText(row []string, columns map[string]int, name string, max int, notes *rowNotes) string {
	cell := normalizeTextCell(cellOf(row, columns, name))
	truncated, cut := truncateCell(cell, max)
	if cut {
		notes.markTruncated(name)
	}
	return truncated
}

// cellNumber 取数值字段：去首尾空白 + 全角转半角 + 超长截断。
//
// 数值字段的上限比文本字段宽得多（这里给 64 字符），
// 因为数值本身不该长；真长到那份上，多半是厂家把单位说明写进了数值列。
func cellNumber(row []string, columns map[string]int, name string, notes *rowNotes) string {
	cell := normalizeNumericCell(cellOf(row, columns, name))
	truncated, cut := truncateCell(cell, maxNumericCellRunes)
	if cut {
		notes.markTruncated(name)
	}
	return truncated
}

// maxNumericCellRunes 是数值单元格的长度上限（字符）。
const maxNumericCellRunes = 64

// rowNotes 收集一行数据的形态标注。
type rowNotes struct {
	// missing 是本行缺测的字段名。
	missing []string
	// truncated 是本行被截断的字段名。
	truncated []string
	// extra 是本行多出来的取值个数（行长度超过表头）。
	extra int
}

// markMissing 记一个缺测字段名（重复记只算一次）。
func (r *rowNotes) markMissing(name string) {
	for _, existing := range r.missing {
		if existing == name {
			return
		}
	}
	r.missing = append(r.missing, name)
}

// markTruncated 记一个被截断的字段名（重复记只算一次）。
func (r *rowNotes) markTruncated(name string) {
	for _, existing := range r.truncated {
		if existing == name {
			return
		}
	}
	r.truncated = append(r.truncated, name)
}

// summary 汇总成领域层的形态标注。
//
// 字段名排序后输出：同一份文件反复导入，标注内容应当逐字节一致，
// 否则「这批数据有没有被动过」就没法用文件比对来回答了。
func (r *rowNotes) summary(encoding string, fallback bool) domain.DataNotes {
	notes := domain.DataNotes{
		Encoding:         encoding,
		EncodingFallback: fallback,
		ExtraValues:      r.extra,
		Truncated:        len(r.truncated) > 0,
	}
	notes.MissingFields = sortedUnique(r.missing)
	notes.TruncatedFields = sortedUnique(r.truncated)
	return notes
}

// sortedUnique 去重并排序。
func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
