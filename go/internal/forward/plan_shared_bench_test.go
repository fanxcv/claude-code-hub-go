package forward

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件量 PlanShared（竞速内复用与候选无关的转换产物）的收益：同一场竞速里三个候选，
// 比对共享与不共享时每轮编译三份计划的分配字节数。
//
// 语料按真实形状构造：约 100 KB 的 claude 流式正文（长 system + 24 条消息 + 12 个工具），
// 全部候选都开转换——不转换时正文就是客户端快照那一片字节，本来就不复制。
//
// 不共享那一档就是改造前的行为（每个 attempt 各自解析、解码、编码一遍）。

func benchRaceClientBody() []byte {
	var builder strings.Builder
	builder.WriteString(`{"model":"claude-sonnet-4-5-20250929","max_tokens":4096,"stream":true,"system":"`)
	builder.WriteString(strings.Repeat("你是网关助手，请严格遵循指令。", 400))
	builder.WriteString(`","messages":[`)
	for index := 0; index < 24; index++ {
		if index > 0 {
			builder.WriteString(",")
		}
		fmt.Fprintf(&builder, `{"role":"user","content":"%s"}`, strings.Repeat("给我一个详尽的答案。", 60))
	}
	builder.WriteString(`],"tools":[`)
	for index := 0; index < 12; index++ {
		if index > 0 {
			builder.WriteString(",")
		}
		fmt.Fprintf(&builder, `{"name":"tool_%d","description":"%s","input_schema":{"type":"object",`+
			`"properties":{"a":{"type":"string"},"b":{"type":"integer"}}}}`, index, strings.Repeat("工具说明。", 20))
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func benchRaceClient() ClientRequest {
	return ClientRequest{
		Method:  "POST",
		Path:    "/v1/messages",
		Headers: newClientHeaders("content-type", "application/json"),
		Format:  convert.FormatClaude,
		Model:   "claude-sonnet-4-5-20250929",
		Body:    benchRaceClientBody(),
		HasBody: true,
	}
}

func benchRaceProvider(id int64, providerType convert.ProviderType) Provider {
	return Provider{
		ID:   id,
		Name: "候选",
		Type: providerType,
		Key:  "sk-up",
		URL:  "https://relay.example.com",
	}
}

// benchRaceRedirectProvider 是「同协议线、不同重定向目标」的候选：编码键里的模型名不同，
// 故它们**不能**共用同一份正文——这一档量的是键变宽后的上界（三份产物 + 三次编码）。
func benchRaceRedirectProvider(id int64, providerType convert.ProviderType, target string) Provider {
	provider := benchRaceProvider(id, providerType)
	provider.ModelRedirects = []byte(`{"claude-sonnet-4-5-20250929":"` + target + `"}`)
	return provider
}

// BenchmarkPlanRaceCandidates 量「一场竞速编译三个候选的计划」。
func BenchmarkPlanRaceCandidates(b *testing.B) {
	chat := convert.ProviderOpenAICompatible
	responses := convert.ProviderCodex
	anthropic := convert.ProviderClaude
	scenarios := []struct {
		name      string
		providers []Provider
	}{
		{"same-protocol", []Provider{
			benchRaceProvider(1, chat), benchRaceProvider(2, chat), benchRaceProvider(3, chat),
		}},
		{"two-protocols", []Provider{
			benchRaceProvider(1, chat), benchRaceProvider(2, chat), benchRaceProvider(3, responses),
		}},
		{"three-protocols", []Provider{
			benchRaceProvider(1, chat), benchRaceProvider(2, responses), benchRaceProvider(3, anthropic),
		}},
		// 同协议线但重定向目标各异：编码键含模型名，故与不共享持平（上界，不是回归）。
		{"same-protocol-different-redirect", []Provider{
			benchRaceRedirectProvider(1, chat, "upstream-a"),
			benchRaceRedirectProvider(2, chat, "upstream-b"),
			benchRaceRedirectProvider(3, chat, "upstream-c"),
		}},
		// 同协议线、同目标名：键完全相同，仍要共用一份产物（共享仍然生效的那一档）。
		{"same-protocol-same-redirect", []Provider{
			benchRaceRedirectProvider(1, chat, "upstream-a"),
			benchRaceRedirectProvider(2, chat, "upstream-a"),
			benchRaceRedirectProvider(3, chat, "upstream-a"),
		}},
	}
	client := benchRaceClient()
	b.Logf("客户端正文 %d 字节", len(client.Body))
	for _, scenario := range scenarios {
		b.Run(scenario.name+"/unshared", func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				for _, provider := range scenario.providers {
					buildBenchPlan(b, client, provider, nil)
				}
			}
		})
		b.Run(scenario.name+"/shared", func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				shared := NewPlanShared()
				for _, provider := range scenario.providers {
					buildBenchPlan(b, client, provider, shared)
				}
			}
		})
	}
}

func buildBenchPlan(b *testing.B, client ClientRequest, provider Provider, shared *PlanShared) *Plan {
	b.Helper()
	plan, err := BuildPlan(PlanInput{
		Client:            client,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
		Overrides:         NewProviderOverrideApplier(client.Path),
		Shared:            shared,
	})
	if err != nil {
		b.Fatalf("BuildPlan 失败: %v", err)
	}
	return plan
}
