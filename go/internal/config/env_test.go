package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// envMatrixPath 是契约真源，相对本包目录向上三级即仓库根。
const envMatrixPath = "../../../tests/load/env-parity/env-matrix.json"

// lookupMap 把 map 适配成 LookupEnvFunc；未列出的变量即「未设置」。
func lookupMap(values map[string]string) LookupEnvFunc {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func specByName(t *testing.T, name string) envSpec {
	t.Helper()
	for _, spec := range envSpecs {
		if spec.name == name {
			return spec
		}
	}
	t.Fatalf("规格表里没有 %s", name)
	return envSpec{}
}

func fieldByName(t *testing.T, env EnvConfig, field string) reflect.Value {
	t.Helper()
	value := reflect.ValueOf(env).FieldByName(field)
	if !value.IsValid() {
		t.Fatalf("EnvConfig 里没有字段 %s", field)
	}
	return value
}

// text 把字段值渲染成可比较文本：nil 指针渲染为 "<nil>"，其余走 %v。
func text(value reflect.Value) string {
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return "<nil>"
		}
		return fmt.Sprintf("%v", value.Elem().Interface())
	}
	return fmt.Sprintf("%v", value.Interface())
}

// expectedDefaultText 按规格推导默认值的文本形态。
func expectedDefaultText(spec envSpec) string {
	switch spec.kind {
	case kindString, kindEnum:
		return spec.def
	case kindBool:
		return strconv.FormatBool(booleanTransform(spec.def))
	case kindNumber:
		if spec.isInt {
			value, _ := strconv.ParseFloat(spec.def, 64)
			return strconv.FormatInt(int64(value), 10)
		}
		return spec.def
	case kindOptionalNumber:
		// 内层 schema 带 default 时，未设置与空串都取该默认值（zod 的 union 会先命中带默认的那支）。
		if spec.def == "" {
			return "<nil>"
		}
		if spec.isInt {
			value, _ := strconv.ParseFloat(spec.def, 64)
			return strconv.FormatInt(int64(value), 10)
		}
		return spec.def
	default:
		// 其余 optional 类：未设置即 nil。
		return "<nil>"
	}
}

// 三处结构必须一一对应：规格表、结构体字段与标签、对账清单。
// 任一方向多出或少掉一项都说明有变量会静默分叉。
func TestEnvSpecsStructAndParityListAreBijective(t *testing.T) {
	if len(envSpecs) != 70 {
		t.Fatalf("规格表应为 70 项，收到 %d", len(envSpecs))
	}

	specNames := map[string]bool{}
	for _, spec := range envSpecs {
		if specNames[spec.name] {
			t.Fatalf("规格表里 %s 重复", spec.name)
		}
		specNames[spec.name] = true
	}

	// 结构体字段与标签：每个 env 标签都必须在规格表里，且字段名与规格一致。
	fields := reflect.TypeOf(EnvConfig{})
	tagNames := map[string]bool{}
	for i := 0; i < fields.NumField(); i++ {
		field := fields.Field(i)
		tag := field.Tag.Get("env")
		if tag == "" {
			t.Fatalf("字段 %s 缺少 env 标签", field.Name)
		}
		if !specNames[tag] {
			t.Fatalf("字段 %s 的标签 %s 不在规格表里", field.Name, tag)
		}
		if spec := specByName(t, tag); spec.field != field.Name {
			t.Fatalf("%s 的字段名不一致：规格 %s，结构体 %s", tag, spec.field, field.Name)
		}
		if tagNames[tag] {
			t.Fatalf("标签 %s 重复", tag)
		}
		tagNames[tag] = true
	}
	if len(tagNames) != len(envSpecs) {
		t.Fatalf("结构体字段数 %d 与规格表项数 %d 不一致", len(tagNames), len(envSpecs))
	}

	// 对账清单与规格表同源，顺序也必须一致。
	names := ParityVariableNames()
	if len(names) != len(envSpecs) {
		t.Fatalf("对账清单应有 %d 项，收到 %d", len(envSpecs), len(names))
	}
	for i, name := range names {
		if name != envSpecs[i].name {
			t.Fatalf("对账清单第 %d 项为 %s，规格表为 %s", i, name, envSpecs[i].name)
		}
	}
}

// 装载器必须真正读过规格表里的每一项，否则「表里有、代码没实现」会静默分叉。
func TestEverySpecIsConsumedByLoader(t *testing.T) {
	var seen []string
	if _, err := loadEnv(lookupMap(nil), &seen); err != nil {
		t.Fatalf("默认值必须可装载: %v", err)
	}
	if len(seen) != len(envSpecs) {
		t.Fatalf("装载器只读了 %d 项，规格表有 %d 项", len(seen), len(envSpecs))
	}
	unique := map[string]bool{}
	for _, name := range seen {
		if unique[name] {
			t.Fatalf("装载器重复读取 %s", name)
		}
		unique[name] = true
	}
	for _, spec := range envSpecs {
		if !unique[spec.name] {
			t.Fatalf("装载器没有读取 %s", spec.name)
		}
	}
}

// 未设置时每个变量都必须落到契约默认值。
func TestEnvDefaultsForEveryVariable(t *testing.T) {
	env, err := LoadEnv(lookupMap(nil))
	if err != nil {
		t.Fatalf("默认值必须可装载: %v", err)
	}
	for _, spec := range envSpecs {
		want := expectedDefaultText(spec)
		if got := text(fieldByName(t, env, spec.field)); got != want {
			t.Errorf("%s 默认值应为 %s，收到 %s", spec.name, want, got)
		}
	}
}

// safeOverride 给出每个变量的合法覆盖值。
//
// 通用规则：有下界的数值取该下界（必与默认值不同），字符串取自定义值，枚举取最后一项，
// 布尔取反。三条跨字段约束涉及的变量必须显式给值，否则会触发 superRefine 而不是本用例要测的解析。
func safeOverride(spec envSpec) string {
	switch spec.name {
	case "STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP":
		// 约束：>= 4 × STREAM_GATE_PREBUFFER_BYTE_CAP（默认 10485760）。
		return "41943040"
	case "DETACHED_STREAM_BUDGET_BYTES":
		// 约束：>= DETACHED_STREAM_METERING_RESERVE_BYTES（默认 16777216）。
		return "16777216"
	case "DETACHED_STREAM_METERING_RESERVE_BYTES":
		// 约束：<= DETACHED_STREAM_BUDGET_BYTES（默认 67108864）；取下界。
		return "65536"
	case "PORT":
		// 契约里无边界，取一个与默认 23000 不同的合法端口。
		return "24567"
	}
	switch spec.kind {
	case kindBool:
		return strconv.FormatBool(!booleanTransform(spec.def))
	case kindEnum:
		return spec.enum[len(spec.enum)-1]
	case kindString, kindOptionalString:
		return "cch-override"
	case kindCredential:
		return "cch-override-credential-16"
	}
	if spec.hasMin {
		return formatBound(spec.min)
	}
	return formatBound(1)
}

// 已设置时必须按契约解析，且只影响该变量。
func TestEnvOverridesForEveryVariable(t *testing.T) {
	for _, spec := range envSpecs {
		t.Run(spec.name, func(t *testing.T) {
			override := safeOverride(spec)
			env, err := LoadEnv(lookupMap(map[string]string{spec.name: override}))
			if err != nil {
				t.Fatalf("%s=%q 应被接受: %v", spec.name, override, err)
			}
			got := text(fieldByName(t, env, spec.field))
			if got != override {
				t.Fatalf("%s=%q 解析为 %s", spec.name, override, got)
			}
			// 其余变量仍取默认值。
			for _, other := range envSpecs {
				if other.name == spec.name {
					continue
				}
				if got := text(fieldByName(t, env, other.field)); got != expectedDefaultText(other) {
					t.Fatalf("覆盖 %s 时 %s 被影响：期望 %s，收到 %s",
						spec.name, other.name, expectedDefaultText(other), got)
				}
			}
		})
	}
}

// 数值变量的越界与非法输入必须被拒，且错误信息带变量名。
func TestEnvNumericBoundsRejection(t *testing.T) {
	for _, spec := range envSpecs {
		if spec.kind != kindNumber && spec.kind != kindOptionalNumber {
			continue
		}
		cases := []struct {
			name  string
			value string
		}{
			{spec.name + " 非数字", "not-a-number"},
		}
		if spec.hasMin {
			cases = append(cases, struct{ name, value string }{
				spec.name + " 低于下界", formatBound(spec.min - 1),
			})
		}
		if spec.hasMax {
			cases = append(cases, struct{ name, value string }{
				spec.name + " 高于上界", formatBound(spec.max + 1),
			})
		}
		if spec.isInt {
			cases = append(cases, struct{ name, value string }{spec.name + " 非整数", "1.5"})
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				_, err := LoadEnv(lookupMap(map[string]string{spec.name: testCase.value}))
				if err == nil {
					t.Fatalf("%s=%q 应被拒绝", spec.name, testCase.value)
				}
				if !strings.Contains(err.Error(), spec.name) {
					t.Fatalf("错误信息必须含变量名 %s，收到 %v", spec.name, err)
				}
			})
		}
	}
}

// 枚举变量的非法取值必须被拒，错误信息列出允许值。
func TestEnvEnumRejection(t *testing.T) {
	for _, spec := range envSpecs {
		if spec.kind != kindEnum {
			continue
		}
		t.Run(spec.name, func(t *testing.T) {
			_, err := LoadEnv(lookupMap(map[string]string{spec.name: "cch-bogus"}))
			if err == nil {
				t.Fatalf("%s=cch-bogus 应被拒绝", spec.name)
			}
			if !strings.Contains(err.Error(), spec.name) {
				t.Fatalf("错误信息必须含变量名，收到 %v", err)
			}
			for _, allowed := range spec.enum {
				if !strings.Contains(err.Error(), allowed) {
					t.Fatalf("错误信息应列出允许值 %s，收到 %v", allowed, err)
				}
			}
		})
	}
}

// booleanTransform 逐字复刻：大小写敏感，且「设为空串」是已设置。
func TestEnvBooleanSemantics(t *testing.T) {
	cases := []struct {
		value    string
		expected bool
	}{
		{"false", false},
		{"0", false},
		{"true", true},
		{"1", true},
		{"", true},
		{"False", true},
		{"no", true},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprintf("AUTO_MIGRATE=%q", testCase.value), func(t *testing.T) {
			env, err := LoadEnv(lookupMap(map[string]string{"AUTO_MIGRATE": testCase.value}))
			if err != nil {
				t.Fatalf("装载失败: %v", err)
			}
			if env.AutoMigrate != testCase.expected {
				t.Fatalf("AUTO_MIGRATE=%q 应为 %v，收到 %v", testCase.value, testCase.expected, env.AutoMigrate)
			}
		})
	}

	// 默认值也走同一个 transform：DEBUG_MODE 默认 "false" → false。
	env, err := LoadEnv(lookupMap(nil))
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if env.DebugMode {
		t.Fatal("DEBUG_MODE 未设置时应为 false")
	}
	if !env.EnableRateLimit {
		t.Fatal("ENABLE_RATE_LIMIT 未设置时应为 true")
	}
}

// optionalNumber 的空串语义：未设置与空串都走 undefined（有默认值则取默认值）。
func TestEnvOptionalNumberEmptyStringSemantics(t *testing.T) {
	env, err := LoadEnv(lookupMap(map[string]string{
		"DB_POOL_IDLE_TIMEOUT":    "",
		"DB_STATEMENT_TIMEOUT_MS": "",
	}))
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if env.DBPoolIdleTimeout != nil {
		t.Fatalf("DB_POOL_IDLE_TIMEOUT 无默认值，空串应为未配置，收到 %v", *env.DBPoolIdleTimeout)
	}
	if env.DBStatementTimeoutMS == nil || *env.DBStatementTimeoutMS != 90000 {
		t.Fatalf("DB_STATEMENT_TIMEOUT_MS 有默认值，空串应取 90000，收到 %v", env.DBStatementTimeoutMS)
	}
}

// 非 optional 的数值变量：空串在 JS 里是 Number("") = 0，本实现保持一致。
func TestEnvNumberEmptyStringCoercesToZero(t *testing.T) {
	env, err := LoadEnv(lookupMap(map[string]string{"PORT": ""}))
	if err != nil {
		t.Fatalf("PORT 空串应被接受（契约里无边界）: %v", err)
	}
	if env.Port != 0 {
		t.Fatalf("PORT 空串应解析为 0，收到 %v", env.Port)
	}
	// 有下界的变量则应在 0 处被拒。
	if _, err := LoadEnv(lookupMap(map[string]string{"AUTH_SESSION_TTL_SECONDS": ""})); err == nil {
		t.Fatal("AUTH_SESSION_TTL_SECONDS 空串应被下界拒绝")
	}
}

// 凭据类：空串与占位符视为未配置，长度不足即拒，且错误信息不回显原文。
func TestEnvCredentialPlaceholdersAndRedaction(t *testing.T) {
	env, err := LoadEnv(lookupMap(map[string]string{
		"ADMIN_TOKEN": "",
		"CSRF_SECRET": "change-me",
		"DSN":         "postgresql://user:password@host:port/db",
	}))
	if err != nil {
		t.Fatalf("占位符与空串应视为未配置: %v", err)
	}
	if env.AdminToken != nil || env.CSRFSecret != nil || env.DSN != nil {
		t.Fatalf("空串与占位符应解析为未配置，收到 %v / %v / %v",
			env.AdminToken, env.CSRFSecret, env.DSN)
	}

	const shortSecret = "short-secret-15"
	if _, err := LoadEnv(lookupMap(map[string]string{"CSRF_SECRET": shortSecret})); err == nil {
		t.Fatal("CSRF_SECRET 不足 16 字符应被拒绝")
	} else if strings.Contains(err.Error(), shortSecret) {
		t.Fatalf("错误信息泄漏了 CSRF_SECRET 原文: %v", err)
	}

	// 已配置时摘要只报「已配置」，不带原文。
	const adminToken = "adm1n-t0ken-value"
	const csrfSecret = "csrf-secret-16-chars"
	env, err = LoadEnv(lookupMap(map[string]string{
		"ADMIN_TOKEN": adminToken,
		"CSRF_SECRET": csrfSecret,
	}))
	if err != nil {
		t.Fatalf("合法凭据应通过: %v", err)
	}
	summary := env.EnvSummary()
	if summary["adminTokenConfigured"] != true || summary["csrfSecretConfigured"] != true {
		t.Fatalf("摘要应报「已配置」，收到 %v", summary)
	}
	rendered := fmt.Sprintf("%v", summary)
	for _, secret := range []string{adminToken, csrfSecret} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("摘要泄漏了凭据: %v", rendered)
		}
	}
}

// superRefine 的两条跨字段约束。
func TestEnvCrossFieldConstraints(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			"计量保留区超过总预算",
			map[string]string{
				"DETACHED_STREAM_BUDGET_BYTES":           "3211264",
				"DETACHED_STREAM_METERING_RESERVE_BYTES": "65536",
			},
			"", // 该组合合法：65536 <= 3211264
		},
		{
			"计量保留区确实超过总预算",
			map[string]string{
				"DETACHED_STREAM_BUDGET_BYTES":           "3211264",
				"DETACHED_STREAM_METERING_RESERVE_BYTES": "16777216",
			},
			"DETACHED_STREAM_METERING_RESERVE_BYTES",
		},
		{
			"全局门禁预算不足四倍单请求上限",
			map[string]string{
				"STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP": "2048",
				"STREAM_GATE_PREBUFFER_BYTE_CAP":        "10485760",
			},
			"STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP",
		},
		{
			"恰好四倍应通过",
			map[string]string{
				"STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP": "41943040",
				"STREAM_GATE_PREBUFFER_BYTE_CAP":        "10485760",
			},
			"",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := LoadEnv(lookupMap(testCase.env))
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("该组合应通过，收到 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("该组合应被拒绝")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("错误信息应含 %s，收到 %v", testCase.want, err)
			}
		})
	}
}

// 对账清单必须与矩阵真源逐名一致（矩阵增删变量时本用例会先红，避免静默漂移）。
func TestParityListMatchesMatrix(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(envMatrixPath))
	if err != nil {
		t.Skipf("矩阵文件不可读（可能是独立模块或子集检出），跳过: %v", err)
	}
	var matrix struct {
		Variables []struct {
			Name string `json:"name"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(raw, &matrix); err != nil {
		t.Fatalf("矩阵文件不是合法 JSON: %v", err)
	}
	fromMatrix := make([]string, 0, len(matrix.Variables))
	for _, variable := range matrix.Variables {
		fromMatrix = append(fromMatrix, variable.Name)
	}
	ours := ParityVariableNames()

	sortedMatrix := append([]string(nil), fromMatrix...)
	sortedOurs := append([]string(nil), ours...)
	sort.Strings(sortedMatrix)
	sort.Strings(sortedOurs)

	if strings.Join(sortedMatrix, ",") != strings.Join(sortedOurs, ",") {
		t.Fatalf("对账清单与矩阵不一致：\n矩阵 %d 项\nGo   %d 项", len(sortedMatrix), len(sortedOurs))
	}
}
