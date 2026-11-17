package ingest

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// 本文件覆盖真实院内记录仪导出文件的六类脏形态：
//
//  1. GBK 编码 —— Windows 上导出的文件十有八九不是 UTF-8；
//  2. 生僻字设备名 —— 中文设备名里带罕见字，回退解码后不能变成乱码；
//  3. 空值 —— 可选列空着要保持缺测，不能拿 0 冒充；
//  4. 多余列 —— 厂家自有列忽略即可，但要计数并报出去；
//  5. 超长字段 —— 截断保护，且截了要留痕；
//  6. 全角数字 —— 数值列全角转半角，字符串列保持原文。
//
// 贯穿全部用例的一条断言：脏数据被显式标注，而不是被静默改写。
// 原始文本（尤其是设备名）必须能从事件体里原样取回。

// gbkEncode 把 UTF-8 文本转成 GBK 字节，模拟 Windows 上记录仪导出的文件。
func gbkEncode(t *testing.T, s string) []byte {
	t.Helper()
	encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("GBK 编码失败：%v", err)
	}
	return encoded
}

// rareDeviceName 是带生僻字的中文设备名。
//
// 「黴」「燊」两字在 GBK 扩展区里（GB2312 没有），正是「Windows 中文版
// 记录仪导出的设备名」的典型形态：解码对了字形完整，解码错了就是乱码。
const rareDeviceName = "疫苗冷藏箱-黴-燊-01号"

// gbkSeriesCSV 是一份 GBK 编码的导出文件内容（先以 UTF-8 写在这里，
// 测试里再转成 GBK 字节）。
const gbkSeriesCSV = `point_id,device_id,device_name,measured_at,temperature_c,unit,humidity_pct,battery_pct,accuracy_c,source,alarm_flag,events
point-vax-freezer-01,device-recorder-0001,` + rareDeviceName + `,2026-10-20T08:00:00+08:00,5.0,C,58,92,0.3,recorder,false,
point-vax-freezer-01,device-recorder-0001,` + rareDeviceName + `,2026-10-20T08:05:00+08:00,9.6,C,61,91,0.3,recorder,true,door_open
`

// TestDecodeSeriesBytes_UTF8DoesNotFallback UTF-8 文件不触发编码回退。
func TestDecodeSeriesBytes_UTF8DoesNotFallback(t *testing.T) {
	decoded, err := DecodeSeriesBytes([]byte(gbkSeriesCSV))
	if err != nil {
		t.Fatalf("UTF-8 文件解码应成功：%v", err)
	}
	if decoded.Encoding != EncodingUTF8 {
		t.Errorf("编码判定 = %q，期望 %q", decoded.Encoding, EncodingUTF8)
	}
	if decoded.Fallback {
		t.Error("UTF-8 文件不应触发 encoding_fallback")
	}
	if decoded.OriginalBytes != len(gbkSeriesCSV) {
		t.Errorf("原始字节数 = %d，期望 %d", decoded.OriginalBytes, len(gbkSeriesCSV))
	}
	if !strings.Contains(decoded.Text, rareDeviceName) {
		t.Errorf("解码文本应保留原设备名，实际 = %q", decoded.Text)
	}
}

// TestDecodeSeriesBytes_GBKFallback GBK 文件回退解析，文本还原正确。
func TestDecodeSeriesBytes_GBKFallback(t *testing.T) {
	raw := gbkEncode(t, gbkSeriesCSV)
	if utf8.Valid(raw) {
		t.Fatal("GBK 字节不应同时是合法 UTF-8，前置条件不成立")
	}

	decoded, err := DecodeSeriesBytes(raw)
	if err != nil {
		t.Fatalf("GBK 文件回退解码应成功：%v", err)
	}
	if decoded.Encoding != EncodingGBK {
		t.Errorf("编码判定 = %q，期望 %q", decoded.Encoding, EncodingGBK)
	}
	if !decoded.Fallback {
		t.Error("GBK 文件应标记 encoding_fallback")
	}
	if decoded.OriginalBytes != len(raw) {
		t.Errorf("原始字节数 = %d，期望 %d", decoded.OriginalBytes, len(raw))
	}
	if !strings.Contains(decoded.Text, rareDeviceName) {
		t.Errorf("回退解码后的设备名应为原文，实际 = %q", decoded.Text)
	}
}

// TestDecodeSeriesBytes_BOMStripped UTF-8 BOM 被去掉，第一个列名仍可识别。
//
// Excel 导出的 CSV 常常带 BOM；不去掉的话表头第一个列名会粘上 BOM，
// 表头校验直接报「缺少必需列 point_id」。
func TestDecodeSeriesBytes_BOMStripped(t *testing.T) {
	raw := append([]byte(utf8BOM), []byte(gbkSeriesCSV)...)

	decoded, err := DecodeSeriesBytes(raw)
	if err != nil {
		t.Fatalf("带 BOM 的 UTF-8 文件解码应成功：%v", err)
	}
	if strings.HasPrefix(decoded.Text, utf8BOM) {
		t.Error("BOM 应被去掉")
	}
	if !strings.HasPrefix(decoded.Text, "point_id,") {
		t.Errorf("首个列名应为 point_id，实际 = %q", decoded.Text[:20])
	}
}

// TestDecodeSeriesBytes_NeitherUTF8NorGBK 既不是 UTF-8 也不是 GBK 时显式报错。
//
// 这类字节（GB18030 扩展区、UTF-16 等）若硬解会得到一片乱码，
// 乱码进库比导不进来更糟——它会让人误以为这批数据是好的。
func TestDecodeSeriesBytes_NeitherUTF8NorGBK(t *testing.T) {
	raw := []byte{0xFF, 0xFE, 0x41, 0x00, 0x42, 0x00}

	_, err := DecodeSeriesBytes(raw)
	if err == nil {
		t.Fatal("既不是 UTF-8 也不是 GBK 的字节应报错")
	}
	e := errcode.As(err)
	if e.Code != errcode.Validation {
		t.Errorf("错误码 = %q，期望 %q", e.Code, errcode.Validation)
	}
	if !strings.Contains(e.Details, "UTF-8") || !strings.Contains(e.Details, "GBK") {
		t.Errorf("错误细节应点明两种编码都试过，实际 = %q", e.Details)
	}
}

// TestParseSeries_GBKFileMarksEncodingFallback 整份 GBK 文件解析后，
// 每条事件都标上 encoding_fallback 与 gbk 编码。
func TestParseSeries_GBKFileMarksEncodingFallback(t *testing.T) {
	raw := gbkEncode(t, gbkSeriesCSV)

	parsed, err := ParseTemperatureSeries(raw)
	if err != nil {
		t.Fatalf("GBK 文件解析应成功：%v", err)
	}
	if len(parsed.Events) != 2 {
		t.Fatalf("解析出 %d 条事件，期望 2 条", len(parsed.Events))
	}
	if parsed.Encoding != EncodingGBK || !parsed.EncodingFallback {
		t.Errorf("文件级编码标注 = %q / fallback=%v，期望 gbk / true",
			parsed.Encoding, parsed.EncodingFallback)
	}
	if parsed.OriginalBytes != len(raw) {
		t.Errorf("原始字节数 = %d，期望 %d", parsed.OriginalBytes, len(raw))
	}
	for i, ev := range parsed.Events {
		if ev.DataNotes.Encoding != EncodingGBK {
			t.Errorf("第 %d 条事件编码标注 = %q，期望 %q",
				i, ev.DataNotes.Encoding, EncodingGBK)
		}
		if !ev.DataNotes.EncodingFallback {
			t.Errorf("第 %d 条事件应标 encoding_fallback=true", i)
		}
	}
}

// TestParseSeries_RareDeviceNamePreserved 生僻字设备名在事件体里原样可取回。
//
// 这是本任务最要紧的一条断言：编码回退如果解错了字，
// 后面追「是哪台冰箱出的问题」就全追不上了。
func TestParseSeries_RareDeviceNamePreserved(t *testing.T) {
	parsed, err := ParseTemperatureSeries(gbkEncode(t, gbkSeriesCSV))
	if err != nil {
		t.Fatalf("GBK 文件解析应成功：%v", err)
	}
	if len(parsed.Events) == 0 {
		t.Fatal("解析结果为空")
	}

	ev := parsed.Events[0]
	if ev.DeviceName != rareDeviceName {
		t.Errorf("设备名 = %q，期望原文 %q", ev.DeviceName, rareDeviceName)
	}
	for _, r := range []rune{'黴', '燊'} {
		if !strings.ContainsRune(ev.DeviceName, r) {
			t.Errorf("设备名应保留生僻字 %q，实际 = %q", r, ev.DeviceName)
		}
	}
	if !utf8.ValidString(ev.DeviceName) {
		t.Error("设备名应是合法 UTF-8 文本")
	}
}

// TestParseSeries_EmptyOptionalValueStaysNil 空值保持缺测，不拿 0 冒充，
// 并把缺测字段名记进 missing_fields。
func TestParseSeries_EmptyOptionalValueStaysNil(t *testing.T) {
	csv := `point_id,device_id,device_name,measured_at,temperature_c,unit,humidity_pct,battery_pct,accuracy_c,source,alarm_flag,events
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,,92,0.3,recorder,false,
`

	parsed, err := ParseTemperatureSeries([]byte(csv))
	if err != nil {
		t.Fatalf("解析应成功：%v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("解析出 %d 条事件，期望 1 条", len(parsed.Events))
	}

	ev := parsed.Events[0]
	if ev.HumidityPct != nil {
		t.Errorf("空湿度应是缺测（nil），实际 = %v", *ev.HumidityPct)
	}
	if !containsString(ev.DataNotes.MissingFields, "humidity_pct") {
		t.Errorf("missing_fields = %v，应含 humidity_pct", ev.DataNotes.MissingFields)
	}
	if ev.DataNotes.Truncated {
		t.Error("空值不算截断")
	}
}

// TestParseSeries_MissingOptionalColumnMarksMissingFields 整列缺失时，
// 该列对全程缺测，并出现在每个事件的 missing_fields 与文件级缺列清单里。
func TestParseSeries_MissingOptionalColumnMarksMissingFields(t *testing.T) {
	csv := `point_id,device_id,device_name,measured_at,temperature_c,source
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,recorder
`

	parsed, err := ParseTemperatureSeries([]byte(csv))
	if err != nil {
		t.Fatalf("解析应成功：%v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("解析出 %d 条事件，期望 1 条", len(parsed.Events))
	}
	if !containsString(parsed.MissingColumns, "humidity_pct") {
		t.Errorf("文件级缺列 = %v，应含 humidity_pct", parsed.MissingColumns)
	}
	ev := parsed.Events[0]
	if ev.HumidityPct != nil {
		t.Errorf("整列缺失时湿度应是缺测（nil），实际 = %v", *ev.HumidityPct)
	}
	for _, name := range []string{"humidity_pct", "battery_pct", "accuracy_c"} {
		if !containsString(ev.DataNotes.MissingFields, name) {
			t.Errorf("missing_fields = %v，应含 %s", ev.DataNotes.MissingFields, name)
		}
	}
}

// TestParseSeries_ExtraColumnsIgnoredAndCounted 多余的列被忽略但计数并报出列名。
func TestParseSeries_ExtraColumnsIgnoredAndCounted(t *testing.T) {
	csv := `point_id,device_id,device_name,measured_at,temperature_c,unit,source,柜内序号,导出批次
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,C,recorder,7,B2024
`

	parsed, err := ParseTemperatureSeries([]byte(csv))
	if err != nil {
		t.Fatalf("含多余列的文件应能解析：%v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("解析出 %d 条事件，期望 1 条", len(parsed.Events))
	}
	for _, name := range []string{"柜内序号", "导出批次"} {
		if !containsString(parsed.ExtraColumns, name) {
			t.Errorf("多余列 = %v，应含 %s", parsed.ExtraColumns, name)
		}
	}
	// 多余列不进事件体：它们没有对应的领域字段。
	if parsed.Events[0].DataNotes.ExtraValues != 0 {
		t.Errorf("多余列不是多余取值，extra_values 应为 0，实际 = %d",
			parsed.Events[0].DataNotes.ExtraValues)
	}
}

// TestParseSeries_ExtraRowValuesCounted 行内多出来的取值被忽略但计数。
func TestParseSeries_ExtraRowValuesCounted(t *testing.T) {
	csv := `point_id,device_id,device_name,measured_at,temperature_c,source
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,5.0,recorder,多余值一,多余值二
`

	parsed, err := ParseTemperatureSeries([]byte(csv))
	if err != nil {
		t.Fatalf("行内多值应能解析：%v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("解析出 %d 条事件，期望 1 条", len(parsed.Events))
	}
	if parsed.ExtraValues != 2 {
		t.Errorf("extra_values = %d，期望 2", parsed.ExtraValues)
	}
	if parsed.Events[0].DataNotes.ExtraValues != 2 {
		t.Errorf("事件上的 extra_values = %d，期望 2",
			parsed.Events[0].DataNotes.ExtraValues)
	}
}

// TestParseSeries_OverlongFieldTruncated 超长字段被截断并留痕。
func TestParseSeries_OverlongFieldTruncated(t *testing.T) {
	long := strings.Repeat("超长设备名", 60) // 240 字符，远超 128 上限
	csv := "point_id,device_id,device_name,measured_at,temperature_c,source\n" +
		"point-vax-freezer-01,device-recorder-0001," + long +
		",2026-10-20T08:00:00+08:00,5.0,recorder\n"

	parsed, err := ParseTemperatureSeries([]byte(csv))
	if err != nil {
		t.Fatalf("超长字段应能解析（截断而非跳过）：%v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("解析出 %d 条事件，期望 1 条", len(parsed.Events))
	}

	ev := parsed.Events[0]
	if got := len([]rune(ev.DeviceName)); got != maxDeviceNameRunes {
		t.Errorf("设备名长度 = %d 字符，期望截断到 %d", got, maxDeviceNameRunes)
	}
	if !ev.DataNotes.Truncated {
		t.Error("截断后应标 truncated=true")
	}
	if !containsString(ev.DataNotes.TruncatedFields, "device_name") {
		t.Errorf("truncated_fields = %v，应含 device_name", ev.DataNotes.TruncatedFields)
	}
	// 截断按字符切，不能把汉字劈成半个。
	if !utf8.ValidString(ev.DeviceName) {
		t.Error("截断后的设备名应是合法 UTF-8 文本")
	}
}

// TestParseSeries_FullWidthNumbersNormalized 数值列的全角数字转半角后能解析；
// 字符串列的全角字符保持原文不改写。
func TestParseSeries_FullWidthNumbersNormalized(t *testing.T) {
	csv := `point_id,device_id,device_name,measured_at,temperature_c,unit,humidity_pct,source
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库（Ａ区）,2026-10-20T08:00:00+08:00,１２．３,Ｃ,５８,recorder
`

	parsed, err := ParseTemperatureSeries([]byte(csv))
	if err != nil {
		t.Fatalf("全角数值应能解析：%v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("解析出 %d 条事件，期望 1 条（跳过原因：%+v）", len(parsed.Events), parsed.SkipReasons)
	}

	ev := parsed.Events[0]
	if ev.TemperatureC != 12.3 {
		t.Errorf("温度 = %v℃，期望 12.3℃", ev.TemperatureC)
	}
	if ev.HumidityPct == nil || *ev.HumidityPct != 58 {
		t.Errorf("湿度 = %v，期望 58", ev.HumidityPct)
	}
	if ev.Unit != domain.UnitCelsius {
		t.Errorf("单位 = %q，期望 %q", ev.Unit, domain.UnitCelsius)
	}
	// 字符串字段保持原文：全角括号与全角字母都是现场标识的一部分。
	if ev.DeviceName != "疫苗冷藏库（Ａ区）" {
		t.Errorf("设备名 = %q，期望保留全角原文", ev.DeviceName)
	}
}

// TestParseSeries_NormalizedNumbersKeepRawValue 全角数值换算后仍保留原始文本。
func TestParseSeries_NormalizedNumbersKeepRawValue(t *testing.T) {
	csv := `point_id,device_id,device_name,measured_at,temperature_c,unit,source
point-vax-freezer-01,device-recorder-0001,疫苗冷藏库温度记录仪,2026-10-20T08:00:00+08:00,１２．３,Ｃ,recorder
`

	parsed, err := ParseTemperatureSeries([]byte(csv))
	if err != nil {
		t.Fatalf("全角数值应能解析：%v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("解析出 %d 条事件，期望 1 条（跳过原因：%+v）", len(parsed.Events), parsed.SkipReasons)
	}
	ev := parsed.Events[0]
	if ev.TemperatureRaw == nil || *ev.TemperatureRaw != 12.3 {
		t.Errorf("原始温度值 = %v，期望 12.3", ev.TemperatureRaw)
	}
	if ev.UnitRaw != "Ｃ" {
		t.Errorf("原始单位 = %q，期望保留全角原文「Ｃ」", ev.UnitRaw)
	}
}

// TestImportTemperatureSeries_ReportsShapeFacts 导入回执里报告编码、多余列与缺列。
func TestImportTemperatureSeries_ReportsShapeFacts(t *testing.T) {
	st := newImportTestStore(t)
	defer func() { _ = st.Close() }()

	raw := gbkEncode(t, `point_id,device_id,device_name,measured_at,temperature_c,unit,source,柜内序号
point-vax-freezer-01,device-recorder-0001,`+rareDeviceName+`,2026-10-20T08:00:00+08:00,5.0,C,recorder,7
`)

	result, err := ImportTemperatureSeries(context.Background(), st, raw, false)
	if err != nil {
		t.Fatalf("导入应成功：%v", err)
	}
	if result.Imported != 1 {
		t.Errorf("入库条数 = %d，期望 1", result.Imported)
	}
	if result.Encoding != EncodingGBK || !result.EncodingFallback {
		t.Errorf("回执编码 = %q / fallback=%v，期望 gbk / true",
			result.Encoding, result.EncodingFallback)
	}
	if result.OriginalBytes != len(raw) {
		t.Errorf("回执原始字节数 = %d，期望 %d", result.OriginalBytes, len(raw))
	}
	if !containsString(result.ExtraColumns, "柜内序号") {
		t.Errorf("回执多余列 = %v，应含 柜内序号", result.ExtraColumns)
	}
	if !containsString(result.MissingColumns, "humidity_pct") {
		t.Errorf("回执缺列 = %v，应含 humidity_pct", result.MissingColumns)
	}
}

// TestImportTemperatureSeries_NotesPersistedInStore 形态标注随事件落库并可回读。
//
// 事后回看一条采样时，要能知道它是从什么样的数据里来的。
func TestImportTemperatureSeries_NotesPersistedInStore(t *testing.T) {
	st := newImportTestStore(t)
	defer func() { _ = st.Close() }()

	raw := gbkEncode(t, gbkSeriesCSV)
	if _, err := ImportTemperatureSeries(context.Background(), st, raw, false); err != nil {
		t.Fatalf("导入应成功：%v", err)
	}

	events, err := st.EventsInRange(context.Background(),
		"point-vax-freezer-01",
		timeAt("2026-10-20T08:00:00+08:00"),
		timeAt("2026-10-20T08:05:00+08:00"))
	if err != nil {
		t.Fatalf("回读事件失败：%v", err)
	}
	if len(events) != 2 {
		t.Fatalf("回读 %d 条事件，期望 2 条", len(events))
	}

	ev := events[0]
	if !ev.DataNotes.EncodingFallback || ev.DataNotes.Encoding != EncodingGBK {
		t.Errorf("回读的编码标注 = %q / fallback=%v，期望 gbk / true",
			ev.DataNotes.Encoding, ev.DataNotes.EncodingFallback)
	}
	if ev.DeviceName != rareDeviceName {
		t.Errorf("回读的设备名 = %q，期望原文 %q", ev.DeviceName, rareDeviceName)
	}
}

// containsString 判断切片里是否含有某个字符串。
func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// timeAt 解析一个带时区偏移的时间文本。
func timeAt(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		panic("测试用时间解析失败：" + err.Error())
	}
	return parsed
}
