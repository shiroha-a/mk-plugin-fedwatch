package fedwatch

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

/*
 * 管理ページに出す集計。
 *
 * 出すのは「対応を決められる情報」に絞る。連合先の一覧そのものは本体の
 * /instances にあるので、ここでは**今困っているもの**と**最近変わったもの**
 * だけを見せる。
 */

// maxRows bounds one listing.
const maxRows = 100

// federatingCond limits every listing to instances we actually federate with.
//
// **観測している相手をすべて数えても運用の役に立たない。** 一度でも接触すれば
// 記録は残るので、大半は「昔どこかから 1 通届いただけ」の相手になる。落ちて
// 困るのはやり取りが続いている相手だけなので、本体の `federating` と同じ定義
// (followingCount > 0 OR followersCount > 0) で絞る。
//
// **記録そのものは全件残す。** 連合は切れたり戻ったりするので、絞るのは
// 表示のときだけにしておく。
const federatingCond = `(following_count > 0 OR followers_count > 0)`

// downInstance is one instance that is currently not responding.
type downInstance struct {
	Host            string `json:"host"`
	SoftwareName    string `json:"softwareName"`
	SoftwareVersion string `json:"softwareVersion"`
	// Since is when it stopped responding.
	Since time.Time `json:"since"`
	// FollowingCount is how many of their users we follow. **これが 0 でない
	// 相手が落ちていると、こちらの利用者に実害が出ている。**
	FollowingCount int  `json:"followingCount"`
	FollowersCount int  `json:"followersCount"`
	Suspended      bool `json:"suspended"`
	Blocked        bool `json:"blocked"`
}

// softwareCount is one row of the software breakdown.
type softwareCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	// Down is how many of them are not responding.
	Down int `json:"down"`
}

// eventRow is one recorded change.
type eventRow struct {
	Host   string    `json:"host"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// overview assembles the admin page payload.
func overview(c context.Context, db *sql.DB) (map[string]any, error) {
	var total, down, suspended, blocked int
	if err := db.QueryRowContext(c, `
		SELECT
			count(*),
			count(*) FILTER (WHERE not_responding),
			count(*) FILTER (WHERE suspended),
			count(*) FILTER (WHERE blocked)
		FROM instances WHERE `+federatingCond+`
	`).Scan(&total, &down, &suspended, &blocked); err != nil {
		return nil, err
	}

	// 落ちている相手は**影響の大きい順**に並べる。フォローしている人が多い
	// ほど実害が大きいので、そちらを先に見せる。
	downList, err := listDown(c, db)
	if err != nil {
		return nil, err
	}
	software, err := listSoftware(c, db)
	if err != nil {
		return nil, err
	}
	events, err := recentEvents(c, db, "", 30)
	if err != nil {
		return nil, err
	}

	// **max() は行が無くても NULL を返す** (ErrNoRows にはならない) ので
	// NullTime で受ける。time.Time に直接 Scan すると導入直後に必ず失敗する。
	var last sql.NullTime
	var lastRun *time.Time
	if err := db.QueryRowContext(c, `SELECT max(last_seen_at) FROM instances`).Scan(&last); err != nil {
		return nil, err
	}
	if last.Valid {
		lastRun = &last.Time
	}

	return map[string]any{
		"total":     total,
		"down":      down,
		"suspended": suspended,
		"blocked":   blocked,
		"downList":  downList,
		"software":  software,
		"events":    events,
		"lastRun":   lastRun,
	}, nil
}

func listDown(c context.Context, db *sql.DB) ([]downInstance, error) {
	rows, err := db.QueryContext(c, `
		SELECT host, software_name, software_version, state_since,
		       following_count, followers_count, suspended, blocked
		FROM instances
		WHERE not_responding AND `+federatingCond+`
		ORDER BY following_count DESC, followers_count DESC, state_since ASC
		LIMIT $1
	`, maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // 読み捨て

	out := []downInstance{}
	for rows.Next() {
		var d downInstance
		if err := rows.Scan(&d.Host, &d.SoftwareName, &d.SoftwareVersion, &d.Since,
			&d.FollowingCount, &d.FollowersCount, &d.Suspended, &d.Blocked); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func listSoftware(c context.Context, db *sql.DB) ([]softwareCount, error) {
	rows, err := db.QueryContext(c, `
		SELECT
			CASE WHEN software_name = '' THEN '(不明)' ELSE software_name END AS name,
			count(*),
			count(*) FILTER (WHERE not_responding)
		FROM instances WHERE `+federatingCond+`
		GROUP BY 1
		ORDER BY 2 DESC
		LIMIT $1
	`, maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // 読み捨て

	out := []softwareCount{}
	for rows.Next() {
		var s softwareCount
		if err := rows.Scan(&s.Name, &s.Count, &s.Down); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// recentEvents lists what changed, newest first. host が空なら全体を返す。
func recentEvents(c context.Context, db *sql.DB, host string, limit int) ([]eventRow, error) {
	if limit <= 0 || limit > maxRows {
		limit = 30
	}

	// **host は必ずプレースホルダで渡す。** 文字列連結で組むと、管理者しか
	// 叩けない口でも SQL を注入できてしまう。
	query := `SELECT host, kind, detail, at FROM events`
	args := []any{}
	if host != "" {
		// ホストを指定した場合は絞らない。連合が切れた相手の履歴を見たい
		// こともある。
		query += ` WHERE host = $1`
		args = append(args, host)
	} else {
		// 全体を出すときは連合中の相手だけにする。**観測しただけの相手の
		// 変化まで並べると、対応が要るものが埋もれる。**
		query += ` WHERE host IN (SELECT host FROM instances WHERE ` + federatingCond + `)`
	}
	query += ` ORDER BY at DESC, id DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit)

	rows, err := db.QueryContext(c, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // 読み捨て

	out := []eventRow{}
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(&e.Host, &e.Kind, &e.Detail, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
