package ingress

import (
	"errors"
	"math"
	"testing"
)

// laneGSingleImageBody 是 lane G 实测的单图语料正文长度（6.67 MiB 的 base64 正文，6.99 MB）。
// 验收要求给出「同一份正文在系数 6 与 1 两档下的实际记账值」，这里直接用它当语料。
const laneGSingleImageBody int64 = 6_994_739

// admissionAt 构造一份 pressure 已知的准入层，使判定只看字节预算而不受真实堆影响。
func admissionAt(maxInflightBodyBytes, multiplier int64) *Admission {
	return NewAdmission(AdmissionOptions{
		MaxInflightBodyBytes: maxInflightBodyBytes,
		Multiplier:           multiplier,
		Pressure:             fixedPressure(0, 1<<40, true),
	})
}

// recordedBytes 申请一次正文预算并返回闸门看到的记账值（申请后立即归还）。
func recordedBytes(t *testing.T, admission *Admission, bodyBytes int64) int64 {
	t.Helper()
	lease, err := admission.AcquireBody(bodyBytes)
	if err != nil {
		t.Fatalf("申请 %d 字节失败: %v", bodyBytes, err)
	}
	accounted := admission.Stats().Body.ActiveBytes
	lease.Release()
	return accounted
}

// TestInflightMultiplierScalesAccounting 是记账口径的正面钉子：同一份正文，系数 6 时的记账值
// 是系数 1 时的 6 倍。把乘算改坏（恒等）必红。
func TestInflightMultiplierScalesAccounting(t *testing.T) {
	const budget = 48 << 20
	six := recordedBytes(t, admissionAt(budget, 6), laneGSingleImageBody)
	legacy := recordedBytes(t, admissionAt(budget, 1), laneGSingleImageBody)
	t.Logf("正文 %d 字节：系数 6 记账 %d 字节，系数 1 记账 %d 字节", laneGSingleImageBody, six, legacy)

	if six != laneGSingleImageBody*6 {
		t.Fatalf("系数 6 的记账值 = %d，期望 正文 × 6 = %d", six, laneGSingleImageBody*6)
	}
	if legacy != laneGSingleImageBody {
		t.Fatalf("系数 1 的记账值 = %d，期望等于正文 %d", legacy, laneGSingleImageBody)
	}
	if six != legacy*6 {
		t.Fatalf("系数 6 应为系数 1 的 6 倍：%d != %d × 6", six, legacy)
	}
}

// TestInflightMultiplierIdentity 是旧口径的回归钉子：系数 0 或 1 时换算恒等。
//
// 0 在 API 层的含义随构造路径而变（AdmissionOptions.Multiplier 的 0 是「未注入」，
// 即取 env 或出厂 6），因此恒等只由换算函数保证：InflightCostBytes 收到 0/1 一律不加成。
func TestInflightMultiplierIdentity(t *testing.T) {
	for _, multiplier := range []int64{0, 1, -3} {
		if got := InflightCostBytes(1000, multiplier); got != 1000 {
			t.Fatalf("系数 %d 应恒等，收到 %d", multiplier, got)
		}
	}
	if got := InflightCostBytes(1000, 6); got != 6000 {
		t.Fatalf("系数 6 应换算成 6000，收到 %d", got)
	}
}

// TestInflightMultiplierSaturatesOnOverflow 钉住溢出饱和：成本回绕成负数会让闸门恒放行。
func TestInflightMultiplierSaturatesOnOverflow(t *testing.T) {
	const huge = int64(math.MaxInt64 / 2)
	if got := InflightCostBytes(huge, 6); got != math.MaxInt64 {
		t.Fatalf("溢出应饱和到 MaxInt64，收到 %d（负数即为闸门恒放行）", got)
	}

	admission := admissionAt(huge, 6)
	lease, err := admission.AcquireBody(huge)
	if lease != nil {
		t.Fatalf("饱和后的成本远超预算，不应放行: %v", lease)
	}
	if !errors.Is(err, ErrBodyBudgetExhausted) {
		t.Fatalf("错误 = %v，期望 ErrBodyBudgetExhausted", err)
	}
}

// TestMaxInflightDefaultIs160MiB 钉住默认上限：160 MiB × 系数 3 = 480 MiB 记账上限。
func TestMaxInflightDefaultIs160MiB(t *testing.T) {
	if DefaultMaxInflightBodyBytes != 160*1024*1024 {
		t.Fatalf("在途正文默认上限 = %d，期望 160 MiB", DefaultMaxInflightBodyBytes)
	}
	if DefaultInflightBytesMultiplier != 3 {
		t.Fatalf("放大系数默认值 = %d，期望 3", DefaultInflightBytesMultiplier)
	}

	t.Setenv(EnvMaxInflightBodyBytes, "")
	t.Setenv(EnvInflightBytesMultiplier, "")
	admission := DefaultAdmission()
	if got := admission.Stats().Body.MaxBytes; got != DefaultMaxInflightBodyBytes {
		t.Fatalf("出厂准入上限 = %d，期望 160 MiB", got)
	}
	if got := admission.multiplier; got != DefaultInflightBytesMultiplier {
		t.Fatalf("出厂系数 = %d，期望 3", got)
	}
	if got := admission.Memory.staticBytes; got != 480<<20 {
		t.Fatalf("静态上限 = %d，期望 160 MiB × 3 = 480 MiB", got)
	}
}

// TestInflightMultiplierFromEnv 覆盖环境变量注入与非法值回退（config 层对同一变量 fail fast）。
func TestInflightMultiplierFromEnv(t *testing.T) {
	cases := map[string]int{
		"":     DefaultInflightBytesMultiplier,
		"3":    3,
		"0":    DefaultInflightBytesMultiplier,
		"-2":   DefaultInflightBytesMultiplier,
		"2.5":  DefaultInflightBytesMultiplier,
		" 12 ": 12,
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(EnvInflightBytesMultiplier, raw)
			if got := InflightBytesMultiplier(); got != want {
				t.Fatalf("CCH_GO_INFLIGHT_BYTES_MULTIPLIER=%q 解析为 %d，期望 %d", raw, got, want)
			}
		})
	}
}
