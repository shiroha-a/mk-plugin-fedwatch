package fedwatch

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/shiroha-a/mk/plugin/plugintest"
)

// seedHost writes one instance row directly, so tests can place events in the past.
func seedHost(t *testing.T, db *sql.DB, host string, notResponding bool, firstSeen, stateSince time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO instances (host, not_responding, first_seen_at, last_seen_at, state_since,
			software_name, following_count)
		VALUES ($1, $2, $3, now(), $4, 'misskey', 1)
	`, host, notResponding, firstSeen, stateSince); err != nil {
		t.Fatal(err)
	}
}

func seedEvent(t *testing.T, db *sql.DB, host, kind string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO events (host, kind, at) VALUES ($1, $2, $3)`, host, kind, at); err != nil {
		t.Fatal(err)
	}
}

func uptimeDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testDB(t)
	plugintest.New(t).WithName("fedwatch").WithDB(db).Routes(Plugin)
	return db
}

// ずっと応答していれば 100%。
func TestHostStatus_AlwaysUp(t *testing.T) {
	db := uptimeDB(t)
	now := time.Now()
	seedHost(t, db, "a.example", false, now.Add(-60*24*time.Hour), now.Add(-60*24*time.Hour))

	got, err := hostStatusOf(context.Background(), db, "a.example", 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Uptime != 100 || got.Outages != 0 {
		t.Fatalf("応答率: %.1f%% / 障害 %d 回", got.Uptime, got.Outages)
	}
	if got.Days < 29 || got.Days > 31 {
		t.Errorf("測定期間: %.1f 日", got.Days)
	}
}

// 期間の途中で落ちて戻った場合、落ちていた分だけ下がる。
func TestHostStatus_PartialOutage(t *testing.T) {
	db := uptimeDB(t)
	now := time.Now()
	// 100 日前から観測。30 日の期間のうち、ちょうど 3 日ぶん落ちていた。
	seedHost(t, db, "a.example", false, now.Add(-100*24*time.Hour), now.Add(-5*24*time.Hour))
	seedEvent(t, db, "a.example", "down", now.Add(-8*24*time.Hour))
	seedEvent(t, db, "a.example", "up", now.Add(-5*24*time.Hour))

	got, err := hostStatusOf(context.Background(), db, "a.example", 30)
	if err != nil {
		t.Fatal(err)
	}
	// 3/30 = 10% 落ちていた。
	if got.Uptime < 89.5 || got.Uptime > 90.5 {
		t.Fatalf("応答率: %.1f%% (90%% 前後のはず)", got.Uptime)
	}
	if got.Outages != 1 {
		t.Errorf("障害回数: %d", got.Outages)
	}
}

// **今も落ちている分を数えること。** 最後の `down` から現在までは、まだ
// `up` が来ていないので出来事だけ見ていると抜ける。
func TestHostStatus_StillDown(t *testing.T) {
	db := uptimeDB(t)
	now := time.Now()
	seedHost(t, db, "a.example", true, now.Add(-100*24*time.Hour), now.Add(-15*24*time.Hour))
	seedEvent(t, db, "a.example", "down", now.Add(-15*24*time.Hour))

	got, err := hostStatusOf(context.Background(), db, "a.example", 30)
	if err != nil {
		t.Fatal(err)
	}
	// 30 日のうち 15 日落ちている。
	if got.Uptime < 49.5 || got.Uptime > 50.5 {
		t.Fatalf("応答率: %.1f%% (50%% 前後のはず)", got.Uptime)
	}
	if !got.NotResponding {
		t.Error("現在の状態が反映されていない")
	}
}

// **期間より前から落ちていた場合。** その `down` は期間外にあるので拾えない。
// 出来事が 1 つも無いまま「今落ちている」なら、期間中ずっと落ちていたと見る。
func TestHostStatus_DownBeforeWindow(t *testing.T) {
	db := uptimeDB(t)
	now := time.Now()
	seedHost(t, db, "a.example", true, now.Add(-100*24*time.Hour), now.Add(-60*24*time.Hour))
	seedEvent(t, db, "a.example", "down", now.Add(-60*24*time.Hour))

	got, err := hostStatusOf(context.Background(), db, "a.example", 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Uptime != 0 {
		t.Fatalf("応答率: %.1f%% (ずっと落ちているので 0%% のはず)", got.Uptime)
	}
}

// **観測開始より前は測れない。** 遡って 100% と主張すると、入れたばかりの
// インスタンスで全部 100% に見える。
func TestHostStatus_ShortHistory(t *testing.T) {
	db := uptimeDB(t)
	now := time.Now()
	// 観測を始めて 2 日しか経っていない。
	seedHost(t, db, "a.example", false, now.Add(-2*24*time.Hour), now.Add(-2*24*time.Hour))

	got, err := hostStatusOf(context.Background(), db, "a.example", 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Days > 2.5 {
		t.Fatalf("測定期間: %.1f 日 (観測前まで遡っている)", got.Days)
	}
	if got.Uptime != 100 {
		t.Errorf("応答率: %.1f%%", got.Uptime)
	}
}

// 観測していない相手はエラーにしない。連合し始めたばかりなら普通のこと。
func TestHostStatus_Unknown(t *testing.T) {
	db := uptimeDB(t)

	got, err := hostStatusOf(context.Background(), db, "nobody.example", 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Known {
		t.Fatalf("知らない相手を既知として返した: %+v", got)
	}
}

// 落ちたり戻ったりを繰り返している相手。回数も出す。
func TestHostStatus_Flapping(t *testing.T) {
	db := uptimeDB(t)
	now := time.Now()
	seedHost(t, db, "a.example", false, now.Add(-100*24*time.Hour), now.Add(-1*time.Hour))
	for i := range 4 {
		base := time.Duration(i*5+2) * 24 * time.Hour
		seedEvent(t, db, "a.example", "down", now.Add(-base-12*time.Hour))
		seedEvent(t, db, "a.example", "up", now.Add(-base))
	}

	got, err := hostStatusOf(context.Background(), db, "a.example", 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outages != 4 {
		t.Fatalf("障害回数: %d (4 回のはず)", got.Outages)
	}
	// 12 時間 × 4 = 2 日ぶん落ちている。
	if got.Uptime < 92 || got.Uptime > 94 {
		t.Errorf("応答率: %.1f%% (93%% 前後のはず)", got.Uptime)
	}
}

// 期間の指定は範囲を外れたら既定に落とす。
func TestHostStatus_ClampsDays(t *testing.T) {
	db := uptimeDB(t)
	now := time.Now()
	seedHost(t, db, "a.example", false, now.Add(-400*24*time.Hour), now.Add(-400*24*time.Hour))

	for _, days := range []int{0, -1, 9999} {
		got, err := hostStatusOf(context.Background(), db, "a.example", days)
		if err != nil {
			t.Fatal(err)
		}
		if got.Days < 29 || got.Days > 31 {
			t.Errorf("days=%d -> 測定期間 %.1f 日 (既定の 30 日に落ちていない)", days, got.Days)
		}
	}
}

func TestRound1(t *testing.T) {
	cases := map[float64]float64{
		99.99:  99.9, // **切り上げない。** 落ちていたのに 100% と出す方が困る
		100:    100,
		0:      0,
		-0.001: 0,
		93.456: 93.4,
	}
	for in, want := range cases {
		if got := round1(in); got != want {
			t.Errorf("round1(%v) = %v (期待 %v)", in, got, want)
		}
	}
}

// ルートからも引けること。権限は他と同じく必須。
func TestRoutes_Host(t *testing.T) {
	db, routes := setup(t, &fakeAPI{})
	now := time.Now()
	seedHost(t, db, "a.example", false, now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour))

	if _, err := routes.Call(t, "POST /host", plugintest.Request{
		UserID: "u1", Body: `{"host":"a.example"}`,
	}); err == nil {
		t.Fatal("一般利用者を通した")
	}

	res, err := routes.Call(t, "POST /host", plugintest.Request{
		UserID: "u1", Moderator: true, Body: `{"host":"a.example","days":7}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.(*hostStatus)
	if !got.Known || got.Uptime != 100 {
		t.Fatalf("想定と違う: %+v", got)
	}
	if got.Days < 6.5 || got.Days > 7.5 {
		t.Errorf("指定した期間が効いていない: %.1f 日", got.Days)
	}

	// host が無ければ弾く。
	if _, err := routes.Call(t, "POST /host", plugintest.Request{
		UserID: "u1", Moderator: true, Body: `{}`,
	}); err == nil {
		t.Error("host 無しを通した")
	}
}

// 集計が失敗したら握り潰さない。0% と出すと「落ちている」ように見えてしまう。
//
// **testDB は schema を作り直すので、1 つのテストで 2 回呼ばない。** 呼ぶと
// 先に作った側のテーブルが消える (実際にそれで落ちた)。サブテストごとに
// 分けて、それぞれで用意する。
func TestHostStatus_DBError(t *testing.T) {
	t.Run("状態が読めない", func(t *testing.T) {
		db := uptimeDB(t)
		seedHost(t, db, "a.example", false, time.Now().Add(-40*24*time.Hour), time.Now())
		if _, err := db.Exec(`ALTER TABLE instances DROP COLUMN state_since`); err != nil {
			t.Fatal(err)
		}
		if _, err := hostStatusOf(context.Background(), db, "a.example", 30); err == nil {
			t.Error("DB エラーを握り潰している")
		}
	})

	t.Run("出来事が読めない", func(t *testing.T) {
		db := uptimeDB(t)
		seedHost(t, db, "a.example", false, time.Now().Add(-40*24*time.Hour), time.Now())
		if _, err := db.Exec(`DROP TABLE events`); err != nil {
			t.Fatal(err)
		}
		if _, err := hostStatusOf(context.Background(), db, "a.example", 30); err == nil {
			t.Error("DB エラーを握り潰している")
		}
	})
}
