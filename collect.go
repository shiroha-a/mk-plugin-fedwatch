package fedwatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shiroha-a/mk/plugin"
)

/*
 * 本体の federation/instances を読んで、状態が変わったものだけ記録する。
 *
 * **毎回の観測を全部残さない。** 連合先が数千あるインスタンスで 1 時間ごとに
 * 全件を積むと、1 年で数千万行になる。知りたいのは「変わった瞬間」なので、
 * 前回と違うときだけ 1 行足す。
 */

// apiInstance is the subset of federation/instances we use.
//
// `softwareName` などは null で来ることがあるが、Go では null を string へ
// デコードしてもエラーにならずゼロ値のまま残るので、そのまま受けてよい。
type apiInstance struct {
	Host            string `json:"host"`
	SoftwareName    string `json:"softwareName"`
	SoftwareVersion string `json:"softwareVersion"`
	IsNotResponding bool   `json:"isNotResponding"`
	IsSuspended     bool   `json:"isSuspended"`
	IsBlocked       bool   `json:"isBlocked"`
	UsersCount      int    `json:"usersCount"`
	NotesCount      int    `json:"notesCount"`
	// FollowingCount はこちらが相手をフォローしている数。落ちたときに
	// どれだけ困るかの目安になる。
	FollowingCount int `json:"followingCount"`
	FollowersCount int `json:"followersCount"`
}

// collect walks federation/instances and records what changed.
func collect(c context.Context, ctx plugin.Context, db *sql.DB, set settings) (int, error) {
	api := ctx.API()
	if api == nil {
		// 本番では必ず配線されている。テストで渡していない場合に落とさない。
		return 0, nil
	}
	// **自分のインスタンスの API を読むだけ。** 外部には出ないので、相手の
	// サーバーに負荷をかけない。
	caller := api.Anonymous()

	var total int
	for offset := 0; offset < set.MaxInstances; offset += set.PageSize {
		raw, err := caller.Call(c, "federation/instances", map[string]any{
			"limit":  set.PageSize,
			"offset": offset,
			// 並びを固定しないと offset ページングが重複・欠落する。
			"sort": "+id",
		})
		if err != nil {
			return total, fmt.Errorf("federation/instances: %w", err)
		}
		var page []apiInstance
		if err := json.Unmarshal(raw, &page); err != nil {
			return total, fmt.Errorf("federation/instances の応答を解釈できません: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, in := range page {
			if in.Host == "" {
				continue
			}
			if err := record(c, db, in); err != nil {
				// 1 件の失敗で走査全体を止めない。次の周回で拾い直せる。
				ctx.Logger().Warn("記録に失敗しました", "host", in.Host, "err", err)
				continue
			}
			total++
		}
		if len(page) < set.PageSize {
			break
		}
	}
	return total, nil
}

// stored is the previous state of one instance.
type stored struct {
	softwareVersion string
	notResponding   bool
	suspended       bool
	blocked         bool
}

// record writes one instance, appending events for what changed.
func record(c context.Context, db *sql.DB, in apiInstance) error {
	tx, err := db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // commit 済みなら no-op

	var prev stored
	err = tx.QueryRowContext(c, `
		SELECT software_version, not_responding, suspended, blocked
		FROM instances WHERE host = $1
	`, in.Host).Scan(&prev.softwareVersion, &prev.notResponding, &prev.suspended, &prev.blocked)

	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return err
	}

	events := diffEvents(isNew, prev, in)
	for _, e := range events {
		if _, err := tx.ExecContext(c,
			`INSERT INTO events (host, kind, detail) VALUES ($1, $2, $3)`,
			in.Host, e.kind, e.detail); err != nil {
			return err
		}
	}

	// 応答状態が変わったときだけ state_since を進める。**変わっていないのに
	// 更新すると「いつから落ちているか」が毎回 now() になって意味を失う。**
	stateChanged := isNew || prev.notResponding != in.IsNotResponding
	if _, err := tx.ExecContext(c, `
		INSERT INTO instances (
			host, software_name, software_version, not_responding, suspended, blocked,
			users_count, notes_count, following_count, followers_count,
			first_seen_at, last_seen_at, state_since)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now(), now(), now())
		ON CONFLICT (host) DO UPDATE SET
			software_name = EXCLUDED.software_name,
			software_version = EXCLUDED.software_version,
			not_responding = EXCLUDED.not_responding,
			suspended = EXCLUDED.suspended,
			blocked = EXCLUDED.blocked,
			users_count = EXCLUDED.users_count,
			notes_count = EXCLUDED.notes_count,
			following_count = EXCLUDED.following_count,
			followers_count = EXCLUDED.followers_count,
			last_seen_at = now(),
			state_since = CASE WHEN $11 THEN now() ELSE instances.state_since END
	`, in.Host, in.SoftwareName, in.SoftwareVersion, in.IsNotResponding, in.IsSuspended,
		in.IsBlocked, in.UsersCount, in.NotesCount, in.FollowingCount, in.FollowersCount,
		stateChanged); err != nil {
		return err
	}

	return tx.Commit()
}

type event struct {
	kind   string
	detail string
}

// diffEvents lists what changed between the stored state and the observation.
func diffEvents(isNew bool, prev stored, in apiInstance) []event {
	if isNew {
		// 初回は状態の変化ではないので down/up は立てない。**立てると導入直後に
		// 「今日 100 件落ちた」と出て、履歴として読めなくなる。**
		return []event{{kind: "appeared", detail: softwareLabel(in)}}
	}

	var out []event
	if prev.notResponding != in.IsNotResponding {
		if in.IsNotResponding {
			out = append(out, event{kind: "down"})
		} else {
			out = append(out, event{kind: "up"})
		}
	}
	if prev.suspended != in.IsSuspended {
		out = append(out, event{kind: boolKind(in.IsSuspended, "suspended", "unsuspended")})
	}
	if prev.blocked != in.IsBlocked {
		out = append(out, event{kind: boolKind(in.IsBlocked, "blocked", "unblocked")})
	}
	// バージョンが上がったことは、落ちた原因を追うときの手がかりになる
	// (更新直後から応答しなくなった、など)。初回観測時の空文字からの遷移は
	// 「上がった」ではないので出さない。
	if prev.softwareVersion != in.SoftwareVersion && prev.softwareVersion != "" && in.SoftwareVersion != "" {
		out = append(out, event{
			kind:   "version",
			detail: prev.softwareVersion + " → " + in.SoftwareVersion,
		})
	}
	return out
}

func boolKind(on bool, whenOn, whenOff string) string {
	if on {
		return whenOn
	}
	return whenOff
}

func softwareLabel(in apiInstance) string {
	if in.SoftwareName == "" {
		return ""
	}
	if in.SoftwareVersion == "" {
		return in.SoftwareName
	}
	return in.SoftwareName + " " + in.SoftwareVersion
}
