package jobs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// newHeadProbeSyncer 起一个假上游，并用计数 transport 记下客户端**实际从响应体读走**的字节数
// 与请求次数。
//
// 计在客户端而不是服务端：httptest 的 handler 会把整段正文写进缓冲区，服务端计数分不出
// 「客户端读完了」与「客户端读到一半就关了」。客户端侧的 Read 才是「有没有下载剩余 32 MB」的判据。
func newHeadProbeSyncer(t *testing.T, storeImpl priceStore, body string) (*PriceSyncer, *readCounter) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	counter := &readCounter{}
	syncer, err := NewPriceSyncer(PriceSyncOptions{
		Pools:  &store.Pools{},
		URL:    server.URL,
		Client: &http.Client{Transport: &countingTransport{base: http.DefaultTransport, counter: counter}},
		store:  storeImpl,
		acquire: func(context.Context) (func(context.Context) error, bool, error) {
			return func(context.Context) error { return nil }, true, nil
		},
	})
	if err != nil {
		t.Fatalf("构造同步器失败: %v", err)
	}
	return syncer, counter
}

// readCounter 记客户端读走的字节数与请求次数。
type readCounter struct {
	bytes    int64
	requests int32
}

func (c *readCounter) read() int64 { return atomic.LoadInt64(&c.bytes) }

// countingTransport 把响应体换成计数 reader（客户端每 Read 一次就累加）。
type countingTransport struct {
	base    http.RoundTripper
	counter *readCounter
}

func (c *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	atomic.AddInt32(&c.counter.requests, 1)
	response, err := c.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &countingBody{ReadCloser: response.Body, counter: c.counter}
	return response, nil
}

type countingBody struct {
	ReadCloser io.ReadCloser
	counter    *readCounter
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	atomic.AddInt64(&b.counter.bytes, int64(n))
	return n, err
}

func (b *countingBody) Close() error { return b.ReadCloser.Close() }

// headProbeTable 造一张正文远大于 4 KiB 的 CPT 表：version 在开头，模型在尾部。
//
// 正文必须大于头读窗口，否则「短路」与「读完」不可区分。
func headProbeTable(version string) string {
	models := make([]string, 0, 200)
	for index := 0; index < 200; index++ {
		models = append(models, fmt.Sprintf(`{"slug": "m%d", "model_name": "model-%d", "vendor": "acme",
			"display_name": "Model %d",
			"pricing": [{"provider": "acme", "official": true, "source": "official",
				"charges": {"prompt": {"price": "1", "unit": "per_M_tokens"}}}]}`, index, index, index))
	}
	return fmt.Sprintf(`{
		"schema": "cchp.pricing-table/v1",
		"version": %q,
		"currency": "USD",
		"refreshed_at": "2026-01-02T03:04:05Z",
		"providers": {"acme": {"name": "Acme"}},
		"models": [%s]
	}`, version, strings.Join(models, ","))
}

// TestPriceSyncHeadProbeSkipsDownloadWhenVersionMatches 钉住一级方案：
// 头读 4 KiB 拿到 version 且指纹与目录一致时，直接关闭响应体，不下载剩余正文。
func TestPriceSyncHeadProbeSkipsDownloadWhenVersionMatches(t *testing.T) {
	body := headProbeTable("v1")
	if len(body) <= cloudPriceHeadProbeBytes {
		t.Fatalf("夹具正文 %d 字节必须大于头读窗口 %d", len(body), cloudPriceHeadProbeBytes)
	}

	fake := newFakePriceStore()
	fake.catalog = &store.CloudPricingCatalogRow{Version: "v1+cvt1", Currency: "USD", ModelCount: 200}
	fake.cloudCount = 200
	syncer, read := newHeadProbeSyncer(t, fake, body)

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if result == nil {
		t.Fatal("短路应返回结果")
	}
	if result.Total != 200 || len(result.Added) != 0 || len(result.Updated) != 0 ||
		len(result.Unchanged) != 0 || len(result.Failed) != 0 || len(result.SkippedConflicts) != 0 {
		t.Fatalf("短路应零分类且 Total 取目录行数, 实际 %+v", result)
	}
	if len(fake.inserts) != 0 || len(fake.upserts) != 0 || fake.deleteKeep != nil {
		t.Fatalf("短路后不应有任何写入: inserts=%v upserts=%v keep=%v",
			fake.inserts, fake.upserts, fake.deleteKeep)
	}
	if len(fake.catalogWrites) != 0 {
		t.Fatalf("短路后不应重写目录")
	}
	if got := read.read(); got > cloudPriceHeadProbeBytes {
		t.Fatalf("短路时只应读头读窗口内的字节：上限 %d，实际读了 %d（正文 %d）",
			cloudPriceHeadProbeBytes, got, len(body))
	}
	if got := atomic.LoadInt32(&read.requests); got != 1 {
		t.Fatalf("短路只应发一次请求（不得关闭后重发），实际 %d 次", got)
	}
}

// TestPriceSyncHeadProbeDownloadsFullBodyWhenVersionDiffers 钉住「版本不同 → 走全量」：
// 头读判出指纹不一致后，同一请求内续读完整正文，不重发请求。
func TestPriceSyncHeadProbeDownloadsFullBodyWhenVersionDiffers(t *testing.T) {
	body := headProbeTable("v2")

	fake := newFakePriceStore()
	fake.catalog = &store.CloudPricingCatalogRow{Version: "v1+cvt1", Currency: "USD", ModelCount: 200}
	fake.cloudCount = 200
	syncer, read := newHeadProbeSyncer(t, fake, body)

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if result == nil || result.Total != 200 {
		t.Fatalf("版本不同必须全量重放, 实际 %+v", result)
	}
	if len(fake.inserts) != 200 {
		t.Fatalf("版本不同应写入 200 行, 实际 %d", len(fake.inserts))
	}
	if len(fake.catalogWrites) != 1 || fake.catalogWrites[0].Version != "v2+cvt1" {
		t.Fatalf("应重写目录且指纹为 v2+cvt1, 实际 %+v", fake.catalogWrites)
	}
	if got := read.read(); got != int64(len(body)) {
		t.Fatalf("版本不同应在同一请求内读完正文 %d 字节, 实际 %d", len(body), got)
	}
	if got := atomic.LoadInt32(&read.requests); got != 1 {
		t.Fatalf("续读全量不得重发请求，实际 %d 次", got)
	}
}

// TestPriceSyncHeadProbeFallsBackToFullDownloadWhenVersionUnreadable 钉住降级：
// 头读窗口内拿不到 version（version 在 4 KiB 之后 / 正文残缺）时退回改动前的全量下载。
func TestPriceSyncHeadProbeFallsBackToFullDownloadWhenVersionUnreadable(t *testing.T) {
	// version 键排在 4 KiB 之后：头读窗口内只有半截 JSON。
	models := make([]string, 0, 200)
	for index := 0; index < 200; index++ {
		models = append(models, fmt.Sprintf(`{"slug": "m%d", "model_name": "model-%d", "vendor": "acme",
			"pricing": [{"provider": "acme", "official": true, "source": "official",
				"charges": {"prompt": {"price": "1", "unit": "per_M_tokens"}}}]}`, index, index))
	}
	body := fmt.Sprintf(`{
		"schema": "cchp.pricing-table/v1",
		"currency": "USD",
		"refreshed_at": "2026-01-02T03:04:05Z",
		"providers": {"acme": {"name": "Acme"}},
		"models": [%s],
		"version": "v1"
	}`, strings.Join(models, ","))
	if strings.Index(body, `"version"`) <= cloudPriceHeadProbeBytes {
		t.Fatalf("夹具里 version 必须排在头读窗口 %d 之后", cloudPriceHeadProbeBytes)
	}

	fake := newFakePriceStore()
	// 目录指纹与正文里的 version 一致且行数也一致：若头读「猜」中就会短路（Unchanged 为空、只读 4 KiB）；
	// 实际必须降级为全量——读完整个正文，再由解析后的原有短路判据得出结论。
	fake.catalog = &store.CloudPricingCatalogRow{Version: "v1+cvt1", Currency: "USD", ModelCount: 200}
	fake.cloudCount = 200
	syncer, read := newHeadProbeSyncer(t, fake, body)

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if got := read.read(); got != int64(len(body)) {
		t.Fatalf("降级后应读完整个正文 %d 字节, 实际 %d", len(body), got)
	}
	if got := atomic.LoadInt32(&read.requests); got != 1 {
		t.Fatalf("降级全量不得重发请求，实际 %d 次", got)
	}
	// 退回现状：走完下载、解析、转换，再到原有的版本短路（此时才可能得出「未变更」与名单）。
	if result == nil || result.Total != 200 || len(result.Unchanged) != 200 {
		t.Fatalf("降级后应走完整路径并由原短路判据作答, 实际 %+v", result)
	}
}
