package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// 本文件把「同步拉取云端 CPT 价格表」暴露给管理面
// （Node 侧 src/lib/price-sync/cloud-price-table.ts:63 fetchCloudPriceTableJson +
// :107 fetchAndParseCloudPriceTable，调用方是 src/app/api/prices/cloud-model-count/route.ts）。
//
// 为什么不复用 PriceSyncer.fetchTable：那个方法是**同步任务**的一环（要 store、要 advisory 锁、
// 要版本短路与整表重放），而这条端点只要「抓一次、解析出模型数与版本号」。把整表同步器拖进
// 请求路径等于让一次管理页点击有机会触发一次全量改价。
//
// 与 fetchTable 逐字一致的三处硬化（同一份对端，不能用两套规矩）：
//  1. **重定向守卫**：最终地址的 scheme/host/path 与预期不符即失败。
//  2. **正文字节上限**（默认 cloudPriceMaxBodyBytes）。
//  3. **空正文即失败**。

// CloudPriceFetchOptions 是同步拉取的装配参数。
type CloudPriceFetchOptions struct {
	// URL 为空时用 defaultCloudPriceTableURL（Node 的 CLOUD_PRICE_TABLE_URL 常量，无环境变量）。
	URL string
	// HTTP 为 nil 时用一个专用客户端（30 秒超时，与 Node 的 FETCH_TIMEOUT_MS 同）。
	HTTP *http.Client
	// MaxBodyBytes 为 0 时用 cloudPriceMaxBodyBytes。
	MaxBodyBytes int64
}

// CloudPriceTableSource 是管理面要的窄接口实现：抓一次并解析成 CPT 表。
type CloudPriceTableSource struct {
	url          string
	client       *http.Client
	maxBodyBytes int64
}

// NewCloudPriceTableSource 建同步拉取入口。
func NewCloudPriceTableSource(options CloudPriceFetchOptions) *CloudPriceTableSource {
	client := options.HTTP
	if client == nil {
		client = &http.Client{
			Timeout: cloudPriceFetchTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        2,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		}
	}
	tableURL := options.URL
	if tableURL == "" {
		tableURL = defaultCloudPriceTableURL
	}
	maxBodyBytes := options.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = cloudPriceMaxBodyBytes
	}
	return &CloudPriceTableSource{url: tableURL, client: client, maxBodyBytes: maxBodyBytes}
}

// FetchCloudPriceTable 拉取并解析云端价格表（Node 的 fetchAndParseCloudPriceTable）。
func (s *CloudPriceTableSource) FetchCloudPriceTable(ctx context.Context) (*CptTable, error) {
	body, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	table, err := ParseCptTable(body)
	if err != nil {
		return nil, fmt.Errorf("价格表解析失败: %w", err)
	}
	return table, nil
}

// fetch 抓正文（与 PriceSyncer.fetchTable 同语义，见文件头的三处硬化）。
func (s *CloudPriceTableSource) fetch(ctx context.Context) ([]byte, error) {
	expected, err := url.Parse(s.url)
	if err != nil {
		return nil, fmt.Errorf("价格表地址非法: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("价格表请求构造失败: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("云端价格表拉取失败：%w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.Request != nil && response.Request.URL != nil {
		final := response.Request.URL
		if final.Scheme != expected.Scheme || final.Host != expected.Host || final.Path != expected.Path {
			return nil, errors.New("云端价格表拉取失败：重定向到非预期地址")
		}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("云端价格表拉取失败：HTTP %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, s.maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("云端价格表拉取失败：%w", err)
	}
	if int64(len(body)) > s.maxBodyBytes {
		return nil, fmt.Errorf("云端价格表拉取失败：正文超过 %d 字节上限", s.maxBodyBytes)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("云端价格表拉取失败：内容为空")
	}
	return body, nil
}
