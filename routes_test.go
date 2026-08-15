package fedwatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shiroha-a/mk/plugin"
	"github.com/shiroha-a/mk/plugin/plugintest"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const testSchema = "plugin_fedwatch_test"

// testDB opens a throwaway schema for one test.
//
// フェイクの DB は使わない。SQL の挙動を模した偽物は本物とずれ、通ったのに
// 本番で落ちる形のテストになる。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		envOr("TEST_DB_HOST", "localhost"), envOr("TEST_DB_PORT", "5432"),
		envOr("TEST_DB_USER", "mk"), envOr("TEST_DB_PASS", "mk"),
		envOr("TEST_DB_NAME", "misskey_test"))

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`,
		`CREATE SCHEMA ` + testSchema,
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	db, err := sql.Open("pgx", base+" search_path="+testSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if a, err := sql.Open("pgx", base); err == nil {
			_, _ = a.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
			_ = a.Close()
		}
	})
	return db
}

// fakeAPI serves canned federation/instances pages.
type fakeAPI struct {
	pages [][]apiInstance
	calls int
}

func (a *fakeAPI) Anonymous() plugin.Caller      { return &fakeCaller{api: a} }
func (a *fakeAPI) AsUser(_ string) plugin.Caller { return &fakeCaller{api: a} }

type fakeCaller struct{ api *fakeAPI }

func (c *fakeCaller) Call(_ context.Context, endpoint string, params any) (json.RawMessage, error) {
	if endpoint != "federation/instances" {
		return nil, fmt.Errorf("想定外の endpoint: %s", endpoint)
	}
	c.api.calls++
	// offset からページを決める。offset ページングが正しく進むかを見るため。
	m, _ := params.(map[string]any)
	offset, _ := m["offset"].(int)
	size, _ := m["limit"].(int)
	if size <= 0 {
		size = 100
	}
	idx := offset / size
	if idx >= len(c.api.pages) {
		return json.Marshal([]apiInstance{})
	}
	return json.Marshal(c.api.pages[idx])
}

// setup wires the plugin against a throwaway schema, handing back the DB so
// tests can assert on what was written.
func setup(t *testing.T, api plugin.API) (*sql.DB, plugintest.Handlers) {
	t.Helper()
	db := testDB(t)
	h := plugintest.New(t).WithName("fedwatch").WithDB(db).WithAPI(api)
	return db, h.Routes(Plugin)
}

// **管理画面に出しただけでは API は誰でも叩ける。** どのルートも必ず弾くこと。
func TestRoutes_RequireModerator(t *testing.T) {
	_, routes := setup(t, &fakeAPI{})

	for _, key := range []string{"POST /overview", "POST /events", "POST /collect"} {
		// 未ログイン
		if _, err := routes.Call(t, key, plugintest.Request{}); err == nil {
			t.Errorf("%s: 未ログインを通した", key)
		}
		// ログインしているだけの利用者
		if _, err := routes.Call(t, key, plugintest.Request{UserID: "u1"}); err == nil {
			t.Errorf("%s: 一般利用者を通した", key)
		}
		// モデレーターなら通る
		if _, err := routes.Call(t, key, plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
			t.Errorf("%s: モデレーターを弾いた: %v", key, err)
		}
	}
}

func TestCollect_RecordsAndDetectsChanges(t *testing.T) {
	api := &fakeAPI{pages: [][]apiInstance{{
		{Host: "a.example", SoftwareName: "misskey", SoftwareVersion: "2026.6.0", FollowingCount: 3},
		{Host: "b.example", SoftwareName: "mastodon", IsNotResponding: true},
	}}}
	db, routes := setup(t, api)

	// 1 回目。初回なので down は立たず appeared だけが残る。
	if _, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
		t.Fatal(err)
	}
	assertEventKinds(t, db, "b.example", []string{"appeared"})

	// 2 回目。a が落ちて、b が復旧し、a のバージョンが上がった。
	api.pages = [][]apiInstance{{
		{Host: "a.example", SoftwareName: "misskey", SoftwareVersion: "2026.7.0", FollowingCount: 3, IsNotResponding: true},
		{Host: "b.example", SoftwareName: "mastodon"},
	}}
	if _, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
		t.Fatal(err)
	}
	// 同じ取り込みで起きた出来事は at が同一になるので、id の降順 (記録した
	// 順の逆) で並ぶ。
	assertEventKinds(t, db, "a.example", []string{"version", "down", "appeared"})
	assertEventKinds(t, db, "b.example", []string{"up", "appeared"})

	// 3 回目。何も変わっていないので出来事は増えない。
	if _, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
		t.Fatal(err)
	}
	assertEventKinds(t, db, "a.example", []string{"version", "down", "appeared"})
}

// **状態が変わっていないのに state_since を進めない。** 進めると
// 「いつから落ちているか」が毎回 now() になって意味を失う。
func TestCollect_KeepsStateSince(t *testing.T) {
	api := &fakeAPI{pages: [][]apiInstance{{
		{Host: "a.example", IsNotResponding: true},
	}}}
	db, routes := setup(t, api)

	if _, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
		t.Fatal(err)
	}
	// 落ちた時刻を過去にずらして、再取り込みで戻らないことを見る。
	if _, err := db.Exec(`UPDATE instances SET state_since = now() - interval '3 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
		t.Fatal(err)
	}

	var days float64
	if err := db.QueryRow(
		`SELECT extract(epoch from now() - state_since) / 86400 FROM instances WHERE host = 'a.example'`,
	).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if days < 2.5 {
		t.Fatalf("落ちた時刻が上書きされている (%.1f 日前になっている)", days)
	}
}

// ページングが進むこと。1 ページで打ち切ると連合先の大半を取りこぼす。
func TestCollect_Paginates(t *testing.T) {
	full := make([]apiInstance, 100)
	for i := range full {
		full[i] = apiInstance{Host: fmt.Sprintf("h%d.example", i)}
	}
	api := &fakeAPI{pages: [][]apiInstance{full, {{Host: "last.example"}}}}
	_, routes := setup(t, api)

	res, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.(map[string]any)["collected"]; got != 101 {
		t.Fatalf("取り込み数: %v (2 ページ目まで進んでいない)", got)
	}
}

// **集計は連合中の相手だけを数える。** 一度でも接触すれば記録は残るので、
// 全部数えると「昔どこかから 1 通届いただけ」の相手が大半を占めてしまう。
func TestOverview(t *testing.T) {
	api := &fakeAPI{pages: [][]apiInstance{{
		{Host: "a.example", SoftwareName: "misskey", IsNotResponding: true, FollowingCount: 5},
		// フォロワーだけでも連合中。
		{Host: "b.example", SoftwareName: "misskey", FollowersCount: 2},
		{Host: "c.example", SoftwareName: "mastodon", IsSuspended: true, FollowingCount: 1},
		// どちらも 0 なので数えない。落ちていても実害が無い。
		{Host: "stranger.example", SoftwareName: "mastodon", IsNotResponding: true},
	}}}
	_, routes := setup(t, api)

	if _, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
		t.Fatal(err)
	}
	res, err := routes.Call(t, "POST /overview", plugintest.Request{UserID: "u1", Moderator: true})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)

	if m["total"] != 3 || m["down"] != 1 || m["suspended"] != 1 {
		t.Fatalf("連合していない相手まで数えている: %+v", m)
	}
	down := m["downList"].([]downInstance)
	if len(down) != 1 || down[0].Host != "a.example" || down[0].FollowingCount != 5 {
		t.Errorf("応答なしの一覧に連合していない相手が混ざっている: %+v", down)
	}
	for _, e := range m["events"].([]eventRow) {
		if e.Host == "stranger.example" {
			t.Errorf("連合していない相手の変化を出している: %+v", e)
		}
	}
	software := m["software"].([]softwareCount)
	if len(software) != 2 || software[0].Name != "misskey" || software[0].Count != 2 {
		t.Errorf("ソフトウェア別: %+v", software)
	}
	if m["lastRun"] == nil {
		t.Error("最終取り込み時刻が入っていない")
	}
}

// **まだ 1 度も走っていなくても画面を出せること。** max() は行が無いと
// NULL を返すので、time.Time に直接 Scan すると導入直後に必ず失敗する。
func TestOverview_Empty(t *testing.T) {
	_, routes := setup(t, &fakeAPI{})

	res, err := routes.Call(t, "POST /overview", plugintest.Request{UserID: "u1", Moderator: true})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["total"] != 0 {
		t.Errorf("空でない: %+v", m)
	}
	// **nil の *time.Time は interface に入れると非 nil になる。** 直接
	// 比較しても意味が無いので、実際に frontend が受け取る JSON で見る。
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"lastRun":null`) {
		t.Errorf("走っていないのに時刻がある: %s", encoded)
	}
	// 一覧は null ではなく空配列で返す (frontend が .length で落ちないように)。
	if got := m["downList"].([]downInstance); got == nil {
		t.Error("downList が null")
	}
	if got := m["events"].([]eventRow); got == nil {
		t.Error("events が null")
	}
}

// **host は必ずプレースホルダで渡すこと。** 管理者しか叩けない口でも、
// 文字列連結で組むと SQL を注入できてしまう。
func TestEvents_HostIsParameterised(t *testing.T) {
	api := &fakeAPI{pages: [][]apiInstance{{{Host: "a.example"}}}}
	db, routes := setup(t, api)

	if _, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
		t.Fatal(err)
	}

	injection := `a.example'; DROP TABLE events; --`
	body, _ := json.Marshal(map[string]any{"host": injection})
	res, err := routes.Call(t, "POST /events", plugintest.Request{
		UserID: "u1", Moderator: true, Body: string(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.([]eventRow); len(got) != 0 {
		t.Errorf("一致しない host で行が返った: %+v", got)
	}

	// テーブルが残っていること。
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("events が壊れている: %v", err)
	}
	if n == 0 {
		t.Error("記録が消えている")
	}
}

func TestEvents_FiltersByHost(t *testing.T) {
	api := &fakeAPI{pages: [][]apiInstance{{{Host: "a.example"}, {Host: "b.example"}}}}
	_, routes := setup(t, api)
	if _, err := routes.Call(t, "POST /collect", plugintest.Request{UserID: "u1", Moderator: true}); err != nil {
		t.Fatal(err)
	}

	res, err := routes.Call(t, "POST /events", plugintest.Request{
		UserID: "u1", Moderator: true, Body: `{"host":"a.example"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.([]eventRow)
	if len(got) != 1 || got[0].Host != "a.example" {
		t.Fatalf("絞り込めていない: %+v", got)
	}
}

// --- ジョブ ---

func TestJobs_CollectAndSweep(t *testing.T) {
	db := testDB(t)
	api := &fakeAPI{pages: [][]apiInstance{{{Host: "a.example"}}}}
	harness := plugintest.New(t).WithName("fedwatch").WithDB(db).WithAPI(api).
		WithConfig(map[string]any{"keepDays": 30})

	if err := harness.Jobs(Plugin).Run(t, "collect", ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM instances`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("取り込めていない: n=%d err=%v", n, err)
	}

	// 保持期間を過ぎた出来事は落とす。
	if _, err := db.Exec(`UPDATE events SET at = now() - interval '100 days'`); err != nil {
		t.Fatal(err)
	}
	if err := plugintest.New(t).WithName("fedwatch").WithDB(db).WithAPI(api).
		WithConfig(map[string]any{"keepDays": 30}).Jobs(Plugin).Run(t, "collect", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("古い出来事が残っている: %d", n)
	}
}

func TestJobs_RegistersSchedule(t *testing.T) {
	jobs := plugintest.New(t).WithName("fedwatch").WithDB(testDB(t)).Jobs(Plugin)

	if len(jobs.Schedules) != 1 || jobs.Schedules[0].Name != "collect" {
		t.Fatalf("想定と違う: %+v", jobs.Schedules)
	}
}

// assertEventKinds checks the recorded kinds for one host, newest first.
func assertEventKinds(t *testing.T, db *sql.DB, host string, want []string) {
	t.Helper()
	rows, err := db.Query(`SELECT kind FROM events WHERE host = $1 ORDER BY at DESC, id DESC`, host)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		got = append(got, k)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s の出来事: %v (期待 %v)", host, got, want)
	}
}

// 集計が失敗したら握り潰さずエラーにする。画面に 0 件と出すと、
// 「連合先が無い」のか「読めなかった」のか区別がつかない。
func TestOverview_DBError(t *testing.T) {
	db, _ := setup(t, &fakeAPI{})
	if _, err := db.Exec(`DROP TABLE instances`); err != nil {
		t.Fatal(err)
	}
	if _, err := overview(context.Background(), db); err == nil {
		t.Error("DB エラーを握り潰している")
	}
}

// 集計の途中で失敗した場合も同様。**部分的な結果を返さない** —
// 一覧だけ欠けた画面は、落ちている相手が居ないように見えてしまう。
func TestOverview_PartialFailure(t *testing.T) {
	t.Run("一覧が引けない", func(t *testing.T) {
		db, _ := setup(t, &fakeAPI{})
		if _, err := db.Exec(`ALTER TABLE instances DROP COLUMN state_since`); err != nil {
			t.Fatal(err)
		}
		if _, err := overview(context.Background(), db); err == nil {
			t.Error("一覧の失敗を握り潰している")
		}
	})

	t.Run("出来事が引けない", func(t *testing.T) {
		db, _ := setup(t, &fakeAPI{})
		if _, err := db.Exec(`DROP TABLE events`); err != nil {
			t.Fatal(err)
		}
		if _, err := overview(context.Background(), db); err == nil {
			t.Error("出来事の失敗を握り潰している")
		}
	})
}

// ソフトウェア別の集計が失敗した場合も握り潰さない。
func TestOverview_SoftwareFailure(t *testing.T) {
	db, _ := setup(t, &fakeAPI{})
	if _, err := db.Exec(`ALTER TABLE instances DROP COLUMN software_name`); err != nil {
		t.Fatal(err)
	}
	if _, err := overview(context.Background(), db); err == nil {
		t.Error("DB エラーを握り潰している")
	}
}
