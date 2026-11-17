package domain

import "time"

// HourlyAggregate 是一个冷链点位在一个小时桶里的聚合结果。
//
// 它是「原始事件进冷数据、聚合留热表」这条容量演进路线的落点：
// 多点位 5 分钟粒度持续上报时，一天下来原始采样就能到几万条，
// 全量堆在热表上既拖慢查询，也让库文件无限长。按小时聚合之后，
// 周报与趋势查询看的是聚合行，原始采样按保留期搬进 gzip JSONL 归档，
// 热表只留最近一段。
//
// 三条口径，报表与复核都以它为准：
//   - 事件数：该小时内落库的采样条数（按 measured_at 归桶）。
//   - 越温次数：该小时内「开始的」越温段数（按越限窗口起点归桶）。
//     跨小时的同一段越温只算在它开始的那一小时，不在两小时里各算一次——
//     否则把各小时的越温次数加起来会大于实际段数，周报上的总数对不上。
//   - 峰值 / 平均 / 最低：该小时内温度统计，单位摄氏度。
//
// Archived 表示这一桶的原始采样已被搬进冷数据归档。
// 归档后这一桶不再重算：重算会把「历史峰值」抹成「剩下几条采样的峰值」，
// 趋势图上凭空多出一个下坠的尖峰，那比留着旧值危险得多。
type HourlyAggregate struct {
	// PointID 是点位标识。
	PointID string `json:"point_id"`
	// HourStart 是这一桶的起点（按本地整点切分，库里按 UTC 存）。
	HourStart time.Time `json:"hour_start"`
	// EventCount 是该小时内的采样条数。
	EventCount int `json:"event_count"`
	// ExcursionCount 是该小时内开始的越温段数。
	ExcursionCount int `json:"excursion_count"`
	// PeakTemperatureC 是该小时的最高温度（℃）。
	PeakTemperatureC float64 `json:"peak_temperature_c"`
	// AvgTemperatureC 是该小时的平均温度（℃）。
	AvgTemperatureC float64 `json:"avg_temperature_c"`
	// MinTemperatureC 是该小时的最低温度（℃）。
	MinTemperatureC float64 `json:"min_temperature_c"`
	// Archived 表示这一桶的原始采样已进冷数据归档。
	Archived bool `json:"archived"`
	// UpdatedAt 是这一行最近一次落库的时刻。
	UpdatedAt time.Time `json:"updated_at"`
}
