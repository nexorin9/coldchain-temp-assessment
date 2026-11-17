// 本文件落点位温区配置的登记簿：point_profiles 表与它的读写。
//
// 为什么要有这张表：判定结论的第一问是「这个温区是谁定的」。
// 答不上来的越温结论没法复核，也没法拿给疫苗生产企业评估——
// 而温区恰恰是会变的：换一批制品、换台冷柜、药品说明书改版，
// 温区跟着变。配置只散落在各个人的本地 JSON 文件里时，
// 「这台冷柜现在按什么温区判」就没人答得上来。
//
// 这张表是「点位温区配置的当前口径」，不是判定结论的存档：
// 每条判定 run 落库时把当时用的那份配置快照进 profile_json，
// 历史结论按快照复算，不被后来的改配置影响。
// 两者分工是明确的：表里是「现在按什么判」，快照里是「当时按什么判」。
//
// 三条约定：
//   - 配置只有一份、随时可查：同一点位重复更新是覆盖，不留多份并存的版本。
//   - 不静默改数：写进来的必须是整份配置，不做「只改温区、其余保留」这类
//     局部合并——合并出来的配置没人能复述，也就没人能复核。
//   - 找不到就说找不到：点位没登记配置时回 E_NOT_FOUND，不回一份猜出来的。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
)

// pointProfilesSchemaSQL 是 point_profiles 表的建表脚本。
//
// 全部 IF NOT EXISTS：重复执行即幂等，服务每次启动跑一遍即可。
const pointProfilesSchemaSQL = `
CREATE TABLE IF NOT EXISTS point_profiles (
    point_id     TEXT PRIMARY KEY,
    profile_json TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
`

// upsertPointProfileSQL 是点位温区配置的写入语句。
//
// ON CONFLICT DO UPDATE：同一点位重复更新是覆盖同一行，而不是插出第二行。
// 覆盖时连 updated_at 一起刷新——「这份配置是什么时候改的」要能查。
const upsertPointProfileSQL = `
INSERT INTO point_profiles (point_id, profile_json, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(point_id) DO UPDATE SET
    profile_json = excluded.profile_json,
    updated_at   = excluded.updated_at
`

// preparePointProfiles 建 point_profiles 表（幂等）。
func (s *Store) preparePointProfiles(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, pointProfilesSchemaSQL); err != nil {
		return errcode.Newf(errcode.StorageError, "建 point_profiles 表失败：%v", err)
	}
	return nil
}

// UpsertPointProfile 写入（或覆盖）一个点位的温区配置。
//
// 写入的是整份配置：调用方要改一处，也得把整份交上来，
// 库里不留「半新半旧」的拼装结果。
//
// point_id 为空直接拒绝：没有点位标识的配置挂不到任何冷柜上，
// 写进库只是一条没人找得到的记录。
func (s *Store) UpsertPointProfile(ctx context.Context, profile domain.PointProfile) error {
	pointID := strings.TrimSpace(profile.PointID)
	if pointID == "" {
		return errcode.Newf(errcode.Validation, "点位温区配置缺少 point_id，无法入库")
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return errcode.Newf(errcode.StorageError, "序列化点位温区配置失败：%v", err)
	}
	if _, err := s.db.ExecContext(ctx, upsertPointProfileSQL,
		pointID, string(encoded), time.Now().UTC().Format(tsLayout),
	); err != nil {
		return errcode.Newf(errcode.StorageError, "写入点位温区配置失败：%v", err)
	}
	return nil
}

// PointProfileByID 回读一个点位的温区配置。
//
// 点位未登记时返回 E_NOT_FOUND：调用方据此区分「还没登记」与「库里出问题了」，
// 也据此决定要不要走「配置缺失即不判、转人工」那条路。
func (s *Store) PointProfileByID(ctx context.Context, pointID string) (domain.PointProfile, error) {
	trimmed := strings.TrimSpace(pointID)
	if trimmed == "" {
		return domain.PointProfile{}, errcode.Newf(errcode.Validation, "回读点位温区配置需要 point_id")
	}
	var profileJSON string
	err := s.db.QueryRowContext(ctx,
		"SELECT profile_json FROM point_profiles WHERE point_id = ?", trimmed).Scan(&profileJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return domain.PointProfile{}, errcode.Newf(errcode.NotFound,
				"点位 %s 未登记温区配置，系统不猜测温区、不用默认值顶替", trimmed)
		}
		return domain.PointProfile{}, errcode.Newf(errcode.StorageError,
			"回读点位温区配置失败：%v", err)
	}
	var profile domain.PointProfile
	if err := json.Unmarshal([]byte(profileJSON), &profile); err != nil {
		return domain.PointProfile{}, errcode.Newf(errcode.StorageError,
			"还原点位温区配置失败：%v", err)
	}
	return profile, nil
}

// PointProfiles 回读全部已登记的点位温区配置，按 point_id 升序。
//
// 台账查询端点与「有哪些点位在管」这类盘点都用它：
// 顺序固定，输出可逐字比对，不随库里写入顺序漂移。
func (s *Store) PointProfiles(ctx context.Context) ([]domain.PointProfile, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT profile_json FROM point_profiles ORDER BY point_id ASC")
	if err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读点位温区配置清单失败：%v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]domain.PointProfile, 0, 8)
	for rows.Next() {
		var profileJSON string
		if err := rows.Scan(&profileJSON); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "读取点位温区配置行失败：%v", err)
		}
		var profile domain.PointProfile
		if err := json.Unmarshal([]byte(profileJSON), &profile); err != nil {
			return nil, errcode.Newf(errcode.StorageError, "还原点位温区配置失败：%v", err)
		}
		out = append(out, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.Newf(errcode.StorageError, "回读点位温区配置清单失败：%v", err)
	}
	return out, nil
}
