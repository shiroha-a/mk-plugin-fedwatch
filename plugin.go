// Package fedwatch records how federated instances behave over time and shows
// it on an admin page.
//
// # 本体の /instances と何が違うのか
//
// 本体は**今の状態**しか持たない。「落ちている」ことは分かるが、いつからなのか、
// 前に落ちたのはいつかは分からない。運用で知りたいのはたいてい後者で、
//
//   - 昨日から落ちている相手なのか、さっき落ちたばかりなのか
//   - 落ちたり戻ったりを繰り返しているのか
//   - こちらがフォローしている人がいる相手なのか (影響の大きさ)
//
// が分かると対応を決められる。このプラグインは本体の API を定期的に読んで
// **状態が変わった時だけ**記録し、その履歴を出す。
//
// **外部には一切出ない。** 見るのは自分のインスタンスの API だけなので、
// 相手のサーバーに負荷をかけない。
package fedwatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/elythia-network/elythia/plugin"
)

// Plugin is the entry point referenced by the generated registration code.
var Plugin = plugin.Definition{
	Name:       "fedwatch",
	Version:    "0.1.0",
	APIVersion: plugin.APIVersion,
	Migrations: migrations,
	Routes:     routes,
	Jobs:       jobs,
	// 連合しないプラグインなので nodeinfo には出さない。運営者がどんな拡張を
	// 使っているかは攻撃面の情報になる。
	Peered: false,
}

// settings mirrors the `plugins.fedwatch` section of the instance config.
type settings struct {
	// KeepDays bounds how long events are kept.
	//
	// 全部残すと際限なく増える。運用で振り返るのはせいぜい数か月なので既定は
	// 90 日にしておく。
	KeepDays int `json:"keepDays"`
	// PageSize is how many instances are pulled per API call.
	//
	// 本体の federation/instances は 100 が上限。
	PageSize int `json:"pageSize"`
	// MaxInstances bounds one collection run.
	//
	// 連合先が数万あるインスタンスで 1 回の走査が長引かないよう上限を置く。
	MaxInstances int `json:"maxInstances"`
}

func loadSettings(ctx plugin.Context) (settings, error) {
	s := settings{
		KeepDays:     90,
		PageSize:     100,
		MaxInstances: 5000,
	}
	if err := ctx.Config().Unmarshal(&s); err != nil {
		return s, err
	}
	if s.PageSize <= 0 || s.PageSize > 100 {
		s.PageSize = 100
	}
	return s, nil
}

var migrations = []plugin.Migration{
	{Version: 1, SQL: `
		CREATE TABLE instances (
			host             text PRIMARY KEY,
			software_name    text NOT NULL DEFAULT '',
			software_version text NOT NULL DEFAULT '',
			not_responding   boolean NOT NULL DEFAULT false,
			suspended        boolean NOT NULL DEFAULT false,
			blocked          boolean NOT NULL DEFAULT false,
			users_count      int NOT NULL DEFAULT 0,
			notes_count      int NOT NULL DEFAULT 0,
			-- following_count はこちらが相手をフォローしている数。落ちたときの
			-- 影響の大きさを測るのに使う。
			following_count  int NOT NULL DEFAULT 0,
			followers_count  int NOT NULL DEFAULT 0,
			first_seen_at    timestamptz NOT NULL DEFAULT now(),
			last_seen_at     timestamptz NOT NULL DEFAULT now(),
			-- 応答状態が最後に変わった時刻。「いつから落ちているか」はここから出す。
			state_since      timestamptz NOT NULL DEFAULT now()
		);

		CREATE TABLE events (
			id     bigserial PRIMARY KEY,
			host   text NOT NULL,
			-- down / up / suspended / unsuspended / blocked / unblocked /
			-- version / appeared
			kind   text NOT NULL,
			detail text NOT NULL DEFAULT '',
			at     timestamptz NOT NULL DEFAULT now()
		);
		CREATE INDEX events_at_idx ON events (at DESC);
		CREATE INDEX events_host_idx ON events (host, at DESC);
	`},
}

func routes(ctx plugin.Context, r plugin.Router) error {
	set, err := loadSettings(ctx)
	if err != nil {
		return err
	}
	db := ctx.Storage().DB()

	// **どのルートもモデレーター以上に限る。** 連合先の状態は運用の情報で、
	// 一般の利用者に見せるものではない。管理画面に出しただけでは API は
	// 誰でも叩けるので、ここで必ず弾く。
	guard := func(h func(plugin.Request) (any, error)) plugin.Handler {
		return func(req plugin.Request) (any, error) {
			if !req.IsModerator() {
				return nil, plugin.Errorf(http.StatusForbidden, "権限がありません")
			}
			return h(req)
		}
	}

	r.POST("/overview", guard(func(req plugin.Request) (any, error) {
		return overview(req.Context(), db)
	}))

	// 1 つの相手の応答率。インスタンス情報のページから引く。
	r.POST("/host", guard(func(req plugin.Request) (any, error) {
		var body struct {
			Host string `json:"host"`
			Days int    `json:"days"`
		}
		if err := req.Bind(&body); err != nil || body.Host == "" {
			return nil, plugin.Errorf(http.StatusBadRequest, "host が必要です")
		}
		return hostStatusOf(req.Context(), db, body.Host, body.Days)
	}))

	r.POST("/events", guard(func(req plugin.Request) (any, error) {
		var body struct {
			Host  string `json:"host"`
			Limit int    `json:"limit"`
		}
		_ = req.Bind(&body)
		return recentEvents(req.Context(), db, body.Host, body.Limit)
	}))

	// 手動で走らせる口。定期実行を待たずに今の状態を取り込みたいときに使う。
	r.POST("/collect", guard(func(req plugin.Request) (any, error) {
		n, err := collect(req.Context(), ctx, db, set)
		if err != nil {
			return nil, err
		}
		return map[string]any{"collected": n}, nil
	}))

	return nil
}

func jobs(ctx plugin.Context, j plugin.Jobs) error {
	set, err := loadSettings(ctx)
	if err != nil {
		return err
	}
	db := ctx.Storage().DB()

	j.Handle("collect", func(c context.Context, _ json.RawMessage) error {
		if _, err := collect(c, ctx, db, set); err != nil {
			return err
		}
		return sweep(c, db, set)
	})
	// 1 時間ごと。応答状態はそれより細かく見ても運用の判断は変わらないし、
	// 本体の instance 情報自体がそこまで頻繁には更新されない。
	j.Schedule("7 * * * *", "collect", nil)
	return nil
}

// sweep drops events past the retention window.
func sweep(c context.Context, db *sql.DB, set settings) error {
	_, err := db.ExecContext(c,
		`DELETE FROM events WHERE at < now() - make_interval(days => $1)`, set.KeepDays)
	return err
}
