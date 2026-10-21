package ingress

import (
	"errors"
	"math"
	"testing"
)

// fixedPressure 返回注入的内存读数，便于确定性地测判定边界。
func fixedPressure(inUse, limit uint64, known bool) MemoryPressure {
	return func() (uint64, uint64, bool) { return inUse, limit, known }
}

func TestMemoryGuardAllowBoundaries(t *testing.T) {
	// 上限 1000、比例 0.9 → 可用预算 900。
	cases := []struct {
		name    string
		inUse   uint64
		request int64
		wantErr bool
	}{
		{name: "余量充足", inUse: 100, request: 800},
		{name: "恰好用满预算", inUse: 100, request: 800},
		{name: "超出预算一字节", inUse: 100, request: 801, wantErr: true},
		{name: "已经超过预算", inUse: 950, request: 1, wantErr: true},
		{name: "零申请恒放行", inUse: 100000, request: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := NewMemoryGuard(fixedPressure(tc.inUse, 1000, true), 0.9, 0)
			err := guard.Allow(tc.request)
			if tc.wantErr && err == nil {
				t.Fatalf("期望拒绝，实际放行")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望放行，实际拒绝: %v", err)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrInsufficientMemory) {
					t.Fatalf("错误 = %v，期望 ErrInsufficientMemory", err)
				}
				if StatusOf(err) != StatusUnavailable {
					t.Fatalf("StatusOf = %d，期望 503", StatusOf(err))
				}
			}
		})
	}
}

// TestAllowWithoutMemLimitUsesStaticCap 钉住「GOMEMLIMIT 未设不再无条件放行」：上限不可知时
// 判定退化为静态上限（在途预算 × 系数），超过即拒。改坏 memory.go 的 !known 分支（直接
// return nil）必红。
func TestAllowWithoutMemLimitUsesStaticCap(t *testing.T) {
	const staticCap = 288 << 20
	guard := NewMemoryGuard(fixedPressure(1<<40, 0, false), 0.9, staticCap)
	if err := guard.Allow(staticCap); err != nil {
		t.Fatalf("恰好等于静态上限应放行: %v", err)
	}
	err := guard.Allow(staticCap + 1)
	if err == nil {
		t.Fatal("超过静态上限必须拒：上限不可知不等于无条件放行")
	}
	if !errors.Is(err, ErrInsufficientMemory) || StatusOf(err) != StatusUnavailable {
		t.Fatalf("错误 = %v，期望 ErrInsufficientMemory / 503", err)
	}
	// 堆在用字节再大也不影响静态判定：两者尺度不同，拿它顶上限会变成一律拒绝。
	if err := NewMemoryGuard(fixedPressure(1<<40, 0, false), 0.9, staticCap).Allow(1); err != nil {
		t.Fatalf("静态判定不应与堆在用字节相减: %v", err)
	}
	if _, ok := guard.Headroom(); ok {
		t.Fatal("上限未知时 Headroom 仍应返回 ok=false")
	}
}

func TestMemoryGuardHeadroom(t *testing.T) {
	guard := NewMemoryGuard(fixedPressure(200, 1000, true), 0.9, 0)
	free, ok := guard.Headroom()
	if !ok {
		t.Fatal("上限已知时 ok 应为 true")
	}
	if free != 700 {
		t.Fatalf("余量 = %d，期望 700", free)
	}

	exhausted := NewMemoryGuard(fixedPressure(1000, 1000, true), 0.9, 0)
	free, ok = exhausted.Headroom()
	if !ok || free != 0 {
		t.Fatalf("超限时余量 = %d ok=%v，期望 0 与 true", free, ok)
	}
}

// TestMemoryGuardDefaultRatio 覆盖非法比例回退到 0.9。
func TestMemoryGuardDefaultRatio(t *testing.T) {
	for _, ratio := range []float64{0, -1, 1.5, math.NaN()} {
		guard := NewMemoryGuard(fixedPressure(0, 1000, true), ratio, 0)
		free, _ := guard.Headroom()
		if free != 900 {
			t.Fatalf("比例 %v 未回退到 0.9：余量 = %d", ratio, free)
		}
	}
}

// TestDefaultMemoryPressureReadsRuntime 覆盖真实读数路径：堆在用字节非零，且读数不 panic。
// GOMEMLIMIT 是否设置取决于环境，故只断言两次调用的行为自洽。
func TestDefaultMemoryPressureReadsRuntime(t *testing.T) {
	inUse, limit, known := DefaultMemoryPressure()
	if inUse == 0 {
		t.Fatal("堆在用字节读数为 0，疑似采点不可用")
	}
	if known && limit == 0 {
		t.Fatal("上限已知却为 0")
	}
}

func TestAdmissionAcquireBody(t *testing.T) {
	admission := NewAdmission(AdmissionOptions{
		MaxInflightBodyBytes: 1000,
		// 系数固定 1：本用例钉的是占位记账与归还，按旧口径让「申请 600 + 400 恰好用满 1000」
		// 可读；系数与乘算另有 TestInflightMultiplier* 专钉。
		Multiplier:  1,
		MemoryRatio: 0.9,
		Pressure:    fixedPressure(100, 1<<30, true),
	})

	first, err := admission.AcquireBody(600)
	if err != nil {
		t.Fatalf("首个申请失败: %v", err)
	}
	second, err := admission.AcquireBody(400)
	if err != nil {
		t.Fatalf("第二个申请失败: %v", err)
	}
	overBudget, budgetErr := admission.AcquireBody(1)
	if overBudget != nil {
		t.Fatalf("超预算申请竟然成功: %v", overBudget)
	}
	if !errors.Is(budgetErr, ErrBodyBudgetExhausted) {
		t.Fatalf("超预算错误 = %v，期望 ErrBodyBudgetExhausted", budgetErr)
	}
	if !IsCapacityError(budgetErr) {
		t.Fatalf("容量错误未被识别: %v", budgetErr)
	}
	if StatusOf(budgetErr) != StatusUnavailable {
		t.Fatalf("StatusOf = %d，期望 503", StatusOf(budgetErr))
	}

	first.Release()
	third, err := admission.AcquireBody(1)
	if err != nil {
		t.Fatalf("释放后应可再申请: %v", err)
	}
	third.Release()
	second.Release()

	stats := admission.Stats()
	if stats.Body.ActiveBytes != 0 {
		t.Fatalf("释放后记账未归零: %+v", stats.Body)
	}
	if !stats.MemoryKnown || stats.MemoryFree <= 0 {
		t.Fatalf("内存读数异常: %+v", stats)
	}
}

// TestAdmissionMemoryRejectionTakesPrecedence 覆盖判定顺序：内存不足时即使字节预算充足也拒绝。
func TestAdmissionMemoryRejectionTakesPrecedence(t *testing.T) {
	admission := NewAdmission(AdmissionOptions{
		MaxInflightBodyBytes: 1 << 20,
		Pressure:             fixedPressure(1<<30, 1<<30, true), // 已逼近上限
	})
	_, err := admission.AcquireBody(1024)
	if !errors.Is(err, ErrInsufficientMemory) {
		t.Fatalf("错误 = %v，期望 ErrInsufficientMemory", err)
	}
	if stats := admission.Stats(); stats.Body.ActiveBytes != 0 {
		t.Fatalf("被内存拒绝却占用了字节预算: %+v", stats.Body)
	}
}

// TestAdmissionEnvDefaults 覆盖准入层的环境变量默认值。
func TestAdmissionEnvDefaults(t *testing.T) {
	t.Setenv(EnvMaxInflightBodyBytes, "2048")
	t.Setenv(EnvMaxConcurrentDecompressions, "3")
	t.Setenv(EnvMaxInflightDecompressionByte, "4096")

	admission := DefaultAdmission()
	if got := admission.Body.Stats().MaxBytes; got != 2048 {
		t.Fatalf("Body 上限 = %d，期望 2048", got)
	}
	stats := admission.Decompression.Stats()
	if stats.MaxConcurrent != 3 || stats.MaxBytes != 4096 {
		t.Fatalf("Decompression 上限 = %+v", stats)
	}
	if _, err := admission.AcquireDecompression(4096); err != nil {
		t.Fatalf("申请解压额度失败: %v", err)
	}
}

// TestIsCapacityError 覆盖容量错误的识别范围：体积/损坏错误不属于容量错误。
func TestIsCapacityError(t *testing.T) {
	if IsCapacityError(ErrDecodedTooLarge) {
		t.Fatal("413 不应被判为容量错误")
	}
	if IsCapacityError(ErrCorruptBody) {
		t.Fatal("400 不应被判为容量错误")
	}
	if !IsCapacityError(ErrDecompressionBusy) || !IsCapacityError(ErrInsufficientMemory) {
		t.Fatal("容量错误未被识别")
	}
}
