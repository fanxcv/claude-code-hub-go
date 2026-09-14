package appversion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// envFrom 造一个假环境：只认表里给的名字。
func envFrom(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// 展示形态的规整：统一小写 v、纯数字补 v、其余原样。（本表原在 internal/adminapi，
// 因规整唯一实现已迁到本包，随之一并搬来，避免两份表各自漂。）
func TestDisplayNormalization(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"  ":          "",
		"1.2.3":       "v1.2.3",
		"v1.2.3":      "v1.2.3",
		"V1.2.3":      "v1.2.3",
		"1.2.3-rc.1":  "v1.2.3-rc.1",
		"dev-abc1234": "dev-abc1234",
		"latest":      "latest",
	}
	for input, want := range cases {
		if got := Display(input); got != want {
			t.Errorf("Display(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// 取值链的优先级与规整：APP_VERSION 说了算，其余名字只在它缺席时顶上。
func TestResolveChainPriority(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		want     string
		wantBare string
	}{
		{name: "全空走兜底", env: map[string]string{}, want: Fallback, wantBare: Bare(Fallback)},
		{name: "APP_VERSION 优先于其余两个", env: map[string]string{
			"APP_VERSION": "1.0.1", "CCH_APP_VERSION": "9.9.9", "NEXT_PUBLIC_APP_VERSION": "8.8.8",
		}, want: "v1.0.1", wantBare: "1.0.1"},
		{name: "CCH_APP_VERSION 次之", env: map[string]string{
			"CCH_APP_VERSION": "2.0.0", "NEXT_PUBLIC_APP_VERSION": "8.8.8",
		}, want: "v2.0.0", wantBare: "2.0.0"},
		{name: "NEXT_PUBLIC_APP_VERSION 兼容", env: map[string]string{
			"NEXT_PUBLIC_APP_VERSION": "3.0.0",
		}, want: "v3.0.0", wantBare: "3.0.0"},
		{name: "空白与已带 v 前缀的值", env: map[string]string{
			"APP_VERSION": "  V1.2.3  ",
		}, want: "v1.2.3", wantBare: "1.2.3"},
		{name: "异形值原样保留（只统一 v 前缀这一种规整）", env: map[string]string{
			"APP_VERSION": "release-1.2",
		}, want: "release-1.2", wantBare: "release-1.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(envFrom(tc.env)); got != tc.want {
				t.Errorf("Resolve = %q，期望 %q", got, tc.want)
			}
			if got := ResolveBare(envFrom(tc.env)); got != tc.wantBare {
				t.Errorf("ResolveBare = %q，期望 %q", got, tc.wantBare)
			}
		})
	}
}

// 空白值不得当成注入：`APP_VERSION="  "` 应等同未设置。
func TestResolveTreatsBlankAsAbsent(t *testing.T) {
	got := Resolve(envFrom(map[string]string{"APP_VERSION": "   ", "CCH_APP_VERSION": "4.0.0"}))
	if got != "v4.0.0" {
		t.Errorf("空白 APP_VERSION 应被跳过，实际得到 %q", got)
	}
}

// nil lookup 不得 panic（装配缝允许传 nil）。
func TestResolveNilLookupFallsBack(t *testing.T) {
	if got := Resolve(nil); got != Fallback {
		t.Errorf("nil lookup 应回落到 %q，实际 %q", Fallback, got)
	}
}

// **VERSION 文件不再参与判定**——这是本次修复的核心不变量。
//
// 做法是把自己挪进一个含 VERSION 的临时目录：若哪天有人把文件兜底加回链里，本用例立刻转红。
func TestResolveIgnoresVersionFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("9.9.9\n"), 0o600); err != nil {
		t.Fatalf("写 VERSION 失败: %v", err)
	}
	t.Chdir(dir)

	if got := Resolve(envFrom(map[string]string{})); got != Fallback {
		t.Errorf("cwd 里放着 VERSION(=9.9.9) 时仍应回落到 %q，实际 %q（文件兜底已废弃）", Fallback, got)
	}
}

// 兜底常量必须与仓库 package.json 的 version 同值：两者一旦分叉，页面上的版本号与仓库声明
// 就会各说各话。这条钉子是本次「单一真源」修复在无注入场景下的守门人。
func TestFallbackMatchesPackageJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "package.json"))
	if err != nil {
		t.Fatalf("读 package.json 失败: %v", err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("解析 package.json 失败: %v", err)
	}
	if manifest.Version == "" {
		t.Fatal("package.json 没有 version 字段")
	}
	if Display(manifest.Version) != Fallback {
		t.Errorf("appversion.Fallback = %q，而 package.json 的 version 规整后是 %q；"+
			"两者必须同值（改其一就要改另一）", Fallback, Display(manifest.Version))
	}
}
