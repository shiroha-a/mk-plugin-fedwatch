package fedwatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/shiroha-a/mk/plugin"
	"github.com/shiroha-a/mk/plugin/plugintest"
)

// 初回の観測で down を立てないこと。
//
// **立てると導入直後に「今日 100 件落ちた」と出て履歴として読めなくなる。**
// 落ちているという事実は instances 側の not_responding で分かるので、
// 出来事としては「観測を始めた」だけを残す。
func TestDiffEvents_FirstSighting(t *testing.T) {
	in := apiInstance{Host: "a.example", SoftwareName: "misskey", SoftwareVersion: "2026.7.0", IsNotResponding: true}

	got := diffEvents(true, stored{}, in)
	if len(got) != 1 || got[0].kind != "appeared" {
		t.Fatalf("初回の出来事: %+v", got)
	}
	if got[0].detail != "misskey 2026.7.0" {
		t.Errorf("ソフトウェアが記録されていない: %q", got[0].detail)
	}
}

func TestDiffEvents_Transitions(t *testing.T) {
	cases := []struct {
		name string
		prev stored
		in   apiInstance
		want []string
	}{
		{
			name: "応答しなくなった",
			prev: stored{notResponding: false},
			in:   apiInstance{IsNotResponding: true},
			want: []string{"down"},
		},
		{
			name: "復旧した",
			prev: stored{notResponding: true},
			in:   apiInstance{IsNotResponding: false},
			want: []string{"up"},
		},
		{
			name: "変化なし",
			prev: stored{notResponding: true},
			in:   apiInstance{IsNotResponding: true},
			want: nil,
		},
		{
			name: "停止した",
			prev: stored{},
			in:   apiInstance{IsSuspended: true},
			want: []string{"suspended"},
		},
		{
			name: "ブロックを解除した",
			prev: stored{blocked: true},
			in:   apiInstance{},
			want: []string{"unblocked"},
		},
		{
			name: "同時に複数変わった",
			prev: stored{notResponding: true},
			in:   apiInstance{IsSuspended: true},
			want: []string{"up", "suspended"},
		},
		{
			name: "バージョンが上がった",
			prev: stored{softwareVersion: "2026.6.0"},
			in:   apiInstance{SoftwareVersion: "2026.7.0"},
			want: []string{"version"},
		},
		{
			// 初回観測で空だったものが埋まっただけ。「上がった」ではない。
			name: "バージョンが空から埋まった",
			prev: stored{softwareVersion: ""},
			in:   apiInstance{SoftwareVersion: "2026.7.0"},
			want: nil,
		},
		{
			// 取得元が一時的に返さなくなっただけかもしれない。
			name: "バージョンが空になった",
			prev: stored{softwareVersion: "2026.7.0"},
			in:   apiInstance{SoftwareVersion: ""},
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diffEvents(false, c.prev, c.in)
			if len(got) != len(c.want) {
				t.Fatalf("件数が違う: %+v (期待 %v)", got, c.want)
			}
			for i, want := range c.want {
				if got[i].kind != want {
					t.Errorf("%d 件目: %q (期待 %q)", i, got[i].kind, want)
				}
			}
		})
	}
}

func TestDiffEvents_VersionDetail(t *testing.T) {
	got := diffEvents(false, stored{softwareVersion: "1.0"}, apiInstance{SoftwareVersion: "2.0"})
	if len(got) != 1 || got[0].detail != "1.0 → 2.0" {
		t.Fatalf("前後が分からない: %+v", got)
	}
}

func TestSoftwareLabel(t *testing.T) {
	cases := map[apiInstance]string{
		{SoftwareName: "misskey", SoftwareVersion: "2026.7.0"}: "misskey 2026.7.0",
		{SoftwareName: "misskey"}:                              "misskey",
		{SoftwareVersion: "2026.7.0"}:                          "",
		{}:                                                     "",
	}
	for in, want := range cases {
		if got := softwareLabel(in); got != want {
			t.Errorf("%+v -> %q (期待 %q)", in, got, want)
		}
	}
}

// --- 走査 ---

// collectCtx builds a context whose migrations are applied.
//
// **collect を直接呼ぶときは Routes を通しておく。** テーブルが無いまま呼ぶと
// 1 件ごとに記録が失敗し、その報告のために Logger を触って落ちる。
func collectCtx(t *testing.T, api plugin.API) (plugin.Context, *sql.DB) {
	t.Helper()
	db := testDB(t)
	h := plugintest.New(t).WithName("fedwatch").WithDB(db)
	if api != nil {
		h = h.WithAPI(api)
	}
	h.Routes(Plugin)
	return h.Context(), db
}

// API が配線されていなければ何もしない (テストで渡していない場合に落とさない)。
func TestCollect_NoAPI(t *testing.T) {
	ctx, db := collectCtx(t, nil)
	n, err := collect(context.Background(), ctx, db, settings{PageSize: 100, MaxInstances: 100})
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

// 取得に失敗したら、そこまでの件数とともにエラーを返す。**握り潰さない** —
// 黙って 0 件で終わると「連合先が消えた」ように見える。
func TestCollect_UpstreamError(t *testing.T) {
	ctx, db := collectCtx(t, &failingAPI{})
	_, err := collect(context.Background(), ctx, db, settings{PageSize: 100, MaxInstances: 100})
	if err == nil {
		t.Fatal("取得失敗をエラーにしていない")
	}
}

// 応答が壊れていても同様。
func TestCollect_BrokenResponse(t *testing.T) {
	ctx, db := collectCtx(t, &rawAPI{body: `{not json`})
	_, err := collect(context.Background(), ctx, db, settings{PageSize: 100, MaxInstances: 100})
	if err == nil {
		t.Fatal("壊れた応答を通している")
	}
}

// host が空の行は数えない (主キーに使うので入れられない)。
func TestCollect_SkipsEmptyHost(t *testing.T) {
	ctx, db := collectCtx(t, &fakeAPI{pages: [][]apiInstance{{{Host: ""}, {Host: "a.example"}}}})
	n, err := collect(context.Background(), ctx, db, settings{PageSize: 100, MaxInstances: 100})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("取り込み数: %d (空の host を数えている)", n)
	}
}

type failingAPI struct{}

func (a *failingAPI) Anonymous() plugin.Caller      { return &failingCaller{} }
func (a *failingAPI) AsUser(_ string) plugin.Caller { return &failingCaller{} }

type failingCaller struct{}

func (c *failingCaller) Call(_ context.Context, _ string, _ any) (json.RawMessage, error) {
	return nil, errors.New("upstream down")
}

type rawAPI struct{ body string }

func (a *rawAPI) Anonymous() plugin.Caller      { return &rawCaller{body: a.body} }
func (a *rawAPI) AsUser(_ string) plugin.Caller { return &rawCaller{body: a.body} }

type rawCaller struct{ body string }

func (c *rawCaller) Call(_ context.Context, _ string, _ any) (json.RawMessage, error) {
	return json.RawMessage(c.body), nil
}

// --- 設定 ---

// PageSize は本体の上限 (100) を超えられない。超えた値を渡すと本体が
// invalid param を返して走査ごと失敗する。
func TestLoadSettings_ClampsPageSize(t *testing.T) {
	for _, given := range []int{0, -1, 101, 9999} {
		h := plugintest.New(t).WithName("fedwatch").WithDB(testDB(t)).
			WithConfig(map[string]any{"pageSize": given})
		set, err := loadSettings(h.Context())
		if err != nil {
			t.Fatal(err)
		}
		if set.PageSize != 100 {
			t.Errorf("pageSize=%d -> %d (100 に丸めていない)", given, set.PageSize)
		}
	}

	// 妥当な値はそのまま使う。
	h := plugintest.New(t).WithName("fedwatch").WithDB(testDB(t)).
		WithConfig(map[string]any{"pageSize": 30})
	set, _ := loadSettings(h.Context())
	if set.PageSize != 30 {
		t.Errorf("妥当な値を書き換えた: %d", set.PageSize)
	}
}

// 1 件の記録に失敗しても走査を止めない。次の周回で拾い直せる。
func TestCollect_ContinuesAfterRecordFailure(t *testing.T) {
	ctx, db := collectCtx(t, &fakeAPI{pages: [][]apiInstance{{
		{Host: "a.example"}, {Host: "b.example"},
	}}})
	// 記録先を落として全件を失敗させる。
	if _, err := db.Exec(`DROP TABLE instances`); err != nil {
		t.Fatal(err)
	}

	n, err := collect(context.Background(), ctx, db, settings{PageSize: 100, MaxInstances: 100})
	if err != nil {
		t.Fatalf("1 件の失敗で走査ごと止めている: %v", err)
	}
	if n != 0 {
		t.Errorf("失敗したものを数えている: %d", n)
	}
}

// **走査の上限を守ること。** 連合先が数万あるインスタンスで 1 回の実行が
// 際限なく長引かないようにしている。
func TestCollect_StopsAtMaxInstances(t *testing.T) {
	page := make([]apiInstance, 10)
	for i := range page {
		page[i] = apiInstance{Host: fmt.Sprintf("h%d.example", i)}
	}
	// 同じページを何度でも返す取得元。上限が効いていなければ止まらない。
	api := &fakeAPI{pages: [][]apiInstance{page, page, page, page, page}}
	ctx, db := collectCtx(t, api)

	n, err := collect(context.Background(), ctx, db, settings{PageSize: 10, MaxInstances: 20})
	if err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Fatalf("取り込み数: %d (上限 20 を超えている)", n)
	}
}
