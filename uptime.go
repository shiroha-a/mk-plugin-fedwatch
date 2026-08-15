package fedwatch

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

/*
 * 1 つの相手の応答率。
 *
 * 本体が配送に失敗し続けると `isNotResponding` が立つので、その状態だった時間
 * の割合を出せば「どれだけ届かなかったか」の目安になる。
 *
 * # 過去に遡って積む理由
 *
 * 記録しているのは**状態が変わった瞬間**だけなので、期間の開始時点でどちらだった
 * かは記録に無い。そこで**今の状態から逆向きに遡る**。`down` の出来事に当たれば
 * 「その前は応答していた」、`up` に当たれば「その前は落ちていた」と分かるので、
 * 期間の頭まで確実に辿れる。
 *
 * 期間の頭より前の記録を読まなくて済むのも利点で、何年ぶんの履歴があっても
 * 見るのは期間内の数件だけになる。
 */

// hostStatus is what the instance page shows for one host.
type hostStatus struct {
	Host string `json:"host"`
	// Known is false when we have never observed this host.
	Known           bool   `json:"known"`
	NotResponding   bool   `json:"notResponding"`
	SoftwareName    string `json:"softwareName"`
	SoftwareVersion string `json:"softwareVersion"`
	// Since is when the current state started.
	Since time.Time `json:"since"`
	// Uptime is the share of the measured window the host was responding (0-100).
	Uptime float64 `json:"uptime"`
	// Days is how long we actually measured. **観測を始めたのが最近なら
	// 要求された日数より短くなる。** 「30 日で 100%」と「2 時間で 100%」は
	// 意味が違うので、必ず添えて出す。
	Days float64 `json:"days"`
	// Outages is how many times it stopped responding within the window.
	Outages int `json:"outages"`
}

// defaultUptimeDays is the window we measure by default.
const defaultUptimeDays = 30

// maxUptimeDays bounds what a caller may ask for.
const maxUptimeDays = 365

// hostStatusOf assembles the availability of one host.
func hostStatusOf(c context.Context, db *sql.DB, host string, days int) (*hostStatus, error) {
	if days <= 0 || days > maxUptimeDays {
		days = defaultUptimeDays
	}

	out := &hostStatus{Host: host, Uptime: 100}

	var firstSeen, stateSince time.Time
	err := db.QueryRowContext(c, `
		SELECT not_responding, state_since, first_seen_at, software_name, software_version
		FROM instances WHERE host = $1
	`, host).Scan(&out.NotResponding, &stateSince, &firstSeen, &out.SoftwareName, &out.SoftwareVersion)
	if errors.Is(err, sql.ErrNoRows) {
		// 観測していない相手。**エラーではない** — 連合し始めたばかりなら
		// 普通のこと。表示側はこれを見て何も描かない。
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Known = true
	out.Since = stateSince

	now := time.Now()
	start := now.Add(-time.Duration(days) * 24 * time.Hour)
	// **観測開始より前は測れない。** 遡って 100% と主張すると、入れたばかりの
	// インスタンスで全部 100% に見える。
	if firstSeen.After(start) {
		start = firstSeen
	}
	total := now.Sub(start)
	if total <= 0 {
		out.Days = 0
		return out, nil
	}
	out.Days = total.Hours() / 24

	rows, err := db.QueryContext(c, `
		SELECT kind, at FROM events
		WHERE host = $1 AND kind IN ('down', 'up') AND at >= $2
		ORDER BY at DESC
	`, host, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // 読み捨て

	var downFor time.Duration
	cur := out.NotResponding
	last := now
	for rows.Next() {
		var kind string
		var at time.Time
		if err := rows.Scan(&kind, &at); err != nil {
			return nil, err
		}
		if cur {
			downFor += last.Sub(at)
		}
		if kind == "down" {
			out.Outages++
		}
		// `up` の出来事に当たったなら、その手前は落ちていた。
		cur = kind == "up"
		last = at
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 期間の頭まで残っている分。
	if cur {
		downFor += last.Sub(start)
	}
	// 期間の頭より前から落ちていた場合、その出来事は期間外なので拾えない。
	// 今が落ちていて出来事が 1 つも無ければ、期間中ずっと落ちていたことになる。
	if out.NotResponding && out.Outages == 0 && downFor == 0 {
		downFor = total
	}

	if downFor > total {
		downFor = total
	}
	out.Uptime = round1(100 * (1 - downFor.Seconds()/total.Seconds()))
	return out, nil
}

// round1 keeps one decimal so "99.9%" and "100%" stay distinguishable.
//
// 切り上げない。**落ちていたのに 100% と出す方が困る**ので、下に丸める側で
// 誤差が出るようにする。
func round1(v float64) float64 {
	if v < 0 {
		return 0
	}
	return float64(int64(v*10)) / 10
}
