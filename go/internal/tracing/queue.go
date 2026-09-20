package tracing

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// RecordTerminal 接收一条终态事实。它满足 `terminal.Tracer`。
//
// 语义逐条：
//   - **绝不阻塞**：入队是非阻塞发送，队列满即丢（丢的是可再得的事实，卡住的是响应收尾）。
//   - **绝不 panic**：不关 channel（见 Close），故没有向已关闭 channel 发送的面。
//   - **绝不返回错误**：签名上就没有；失败只计数与记日志。
//   - **与 Close 互斥**：从读关闭位到入队之间持有读锁，Close 拿写锁，故不可能出现
//     「判定时未关闭、发送时后台已退出」——那会让记录落进一个再无消费者的队列。
func (t *Tracer) RecordTerminal(record terminal.TraceRecord) {
	if t == nil {
		return
	}
	if !t.keepSample() {
		t.sampled.Add(1)
		return
	}
	t.sendMu.RLock()
	defer t.sendMu.RUnlock()
	if t.closed {
		return
	}
	select {
	case t.queue <- record:
		t.enqueued.Add(1)
	default:
		dropped := t.dropped.Add(1)
		if dropped == 1 || dropped%dropWarnEvery == 0 {
			t.logWarn("tracing.queue_full", map[string]any{
				"dropped": dropped,
				"queue":   cap(t.queue),
			})
		}
	}
}

// keepSample 按采样率掷点。<=0 全丢、>=1 全留（契约已把取值域钳到 [0,1]，这里只兜底）。
func (t *Tracer) keepSample() bool {
	switch {
	case t.sampleRate <= 0:
		return false
	case t.sampleRate >= 1:
		return true
	default:
		return t.sample() < t.sampleRate
	}
}

// run 是唯一的后台 goroutine：按「批大小或间隔先到者」发一批，收到停止信号后把残余发完即退。
func (t *Tracer) run() {
	defer close(t.done)
	ticker := time.NewTicker(t.flushInterval)
	defer ticker.Stop()

	batch := make([]terminal.TraceRecord, 0, t.batchSize)
	for {
		select {
		case record := <-t.queue:
			batch = append(batch, record)
			if len(batch) >= t.batchSize {
				t.send(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				t.send(batch)
				batch = batch[:0]
			}
		case <-t.stop:
			batch = drain(t.queue, batch, t.batchSize)
			if len(batch) > 0 {
				t.send(batch)
			}
			return
		}
	}
}

// drain 把队列里已入队的事实一次取空（不等待新条目），最多取到 batchSize 条。
//
// 上限是刻意的：收尾时不该为一次巨大的残余阻塞退出（余下的按旁路丢弃）。
func drain(queue <-chan terminal.TraceRecord, batch []terminal.TraceRecord, limit int) []terminal.TraceRecord {
	for len(batch) < limit {
		select {
		case record := <-queue:
			batch = append(batch, record)
		default:
			return batch
		}
	}
	return batch
}

// send 同步发出已组好的一个批次并消化结果。失败只计数与记日志，绝不重试
// （丢一条 trace 只丢一条 trace；重试队列的成本高于它）。
func (t *Tracer) send(batch []terminal.TraceRecord) {
	payload, err := encodeBatch(batch)
	if err != nil {
		t.failed.Add(uint64(len(batch)))
		t.logWarn("tracing.encode_failed", map[string]any{"error": err.Error(), "events": len(batch)})
		return
	}

	// 出站不沿用请求上下文：终态之后客户端可能立刻断开，沿用会把「刚成功的一次请求」
	// 顺带取消掉（与 rollup 旁路同款约束）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(payload))
	if err != nil {
		t.failed.Add(uint64(len(batch)))
		t.logWarn("tracing.request_build_failed", map[string]any{"error": err.Error()})
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Authorization", t.authorization)

	response, err := t.client.Do(request)
	if err != nil {
		t.failed.Add(uint64(len(batch)))
		t.logWarn("tracing.send_failed", map[string]any{"error": err.Error(), "events": len(batch)})
		return
	}
	// 响应体只用于日志线索：读一小段即弃，避免连接无法复用。
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusMultiStatus {
		t.failed.Add(uint64(len(batch)))
		t.logWarn("tracing.send_rejected", map[string]any{
			"status": response.StatusCode,
			"events": len(batch),
		})
		return
	}
	t.sent.Add(uint64(len(batch)))
	if t.debug {
		t.logDebug("tracing.batch_sent", map[string]any{
			"status": response.StatusCode,
			"events": len(batch),
			"bytes":  len(payload),
		})
		// 207 的逐事件结果只在 debug 下看一眼：失败事件即弃，不重投（设计稿 §2）。
		if response.StatusCode == http.StatusMultiStatus {
			t.logDebug("tracing.batch_results", map[string]any{"results": string(body)})
		}
	}
}

func (t *Tracer) logWarn(event string, fields map[string]any) {
	if t == nil || t.logger == nil {
		return
	}
	t.logger.Warn(event, fields)
}

func (t *Tracer) logDebug(event string, fields map[string]any) {
	if t == nil || t.logger == nil {
		return
	}
	t.logger.Debug(event, fields)
}

// gzipPayload 压缩上报体。事件多时收益明显；单条也照压，只为不留两条代码路径。
func gzipPayload(raw []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(raw); err != nil {
		return nil, fmt.Errorf("tracing: gzip 写入失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("tracing: gzip 收尾失败: %w", err)
	}
	return buffer.Bytes(), nil
}
