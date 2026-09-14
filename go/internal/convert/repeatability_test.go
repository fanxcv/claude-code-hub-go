package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// 本文件是「上游 prompt 前缀缓存」的硬判据。
//
// 上游（openai-compatible / anthropic 等）的前缀缓存只认**出站字节**：只要两次请求的
// 正文有任何一处不同，缓存就从分叉点起全部失效，表现为 cache_read_input_tokens 骤降、
// input_tokens 暴涨。
// 因此「同一输入 ⇒ 同一出站字节」是必须被钉死的不变量。两条路径都要钉：
//  1. 每次请求都新解析输入（模拟真实到达）；
//  2. 同一份已解码请求被**重复编码**（模拟重试/竞速对同一条请求的二次发送）。

const repeatabilityRounds = 20

// buildConvertCtx 从原始正文推 ctx，避免各用例各写一份。
func buildConvertCtx(t *testing.T, raw []byte, familyTo string) (WireProtocol, ConvertCtx) {
	t.Helper()
	probe, err := ParseJSON(raw)
	if err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	target := protocolOfFamily(t, familyTo)
	return target, ConvertCtx{
		ClientFormat:   clientFormatOfProtocol(target),
		TargetProto:    target,
		Model:          caseModel(probe),
		Stream:         responsesIsStream(probe),
		ToWireToolName: NormalizeToolName,
	}
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// TestRequestConversionIsRepeatable 钉「每次新解析输入 ⇒ 出站字节恒定」。
func TestRequestConversionIsRepeatable(t *testing.T) {
	file := loadCorpus(t, "requests.json")
	checked := 0
	for _, testCase := range file.Cases {
		if testCase.Passthrough {
			continue
		}
		t.Run(testCase.ID, func(t *testing.T) {
			raw := []byte(testCase.Input)
			sourceProtocol := protocolOfFamily(t, testCase.FamilyFrom)
			target, ctx := buildConvertCtx(t, raw, testCase.FamilyTo)

			var first, firstHash string
			for round := 0; round < repeatabilityRounds; round++ {
				source, err := ParseJSON(raw)
				if err != nil {
					t.Fatalf("第 %d 轮解析失败: %v", round+1, err)
				}
				decoded, ok := DecodeRequest(sourceProtocol, source, ctx)
				if !ok {
					t.Fatalf("协议线 %s 无解码器", sourceProtocol)
				}
				encoded, ok := EncodeRequest(target, decoded.Value, ctx)
				if !ok {
					t.Fatalf("协议线 %s 无编码器", target)
				}
				got := encoded.Body.MarshalCompact()
				currentHash := hashOf(got)
				if round == 0 {
					first, firstHash = got, currentHash
					continue
				}
				if currentHash != firstHash {
					t.Fatalf("第 %d 轮出站体与第 1 轮不同 —— 上游前缀缓存必从分叉点起失效\n"+
						"第 1 轮 sha256=%s\n第 %d 轮 sha256=%s\n%s",
						round+1, firstHash, round+1, currentHash, byteDiff(first, got))
				}
			}
			t.Logf("%s 出站体 sha256=%s", testCase.ID, firstHash)
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("没有跑到任何转换用例")
	}
}

// TestEncodeRequestHasNoHiddenState 钉「同一份已解码请求，编码两次得到同一字节」。
//
// 若编码器会就地改写入参（追加/改写切片、复用内部缓冲），第二次编码就会产出不同的正文。
// 生产含义：重试或竞速对同一条请求二次发送时，第二次出站体与第一次不同 ⇒ 上游缓存
// 无论第一次是否写成功都不会命中，且客户端看到的内容也可能被污染。
func TestEncodeRequestHasNoHiddenState(t *testing.T) {
	file := loadCorpus(t, "requests.json")
	checked := 0
	for _, testCase := range file.Cases {
		if testCase.Passthrough {
			continue
		}
		t.Run(testCase.ID, func(t *testing.T) {
			raw := []byte(testCase.Input)
			sourceProtocol := protocolOfFamily(t, testCase.FamilyFrom)
			target, ctx := buildConvertCtx(t, raw, testCase.FamilyTo)

			source, err := ParseJSON(raw)
			if err != nil {
				t.Fatalf("解析语料失败: %v", err)
			}
			decoded, ok := DecodeRequest(sourceProtocol, source, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无解码器", sourceProtocol)
			}

			firstEncoded, ok := EncodeRequest(target, decoded.Value, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无编码器", target)
			}
			first := firstEncoded.Body.MarshalCompact()

			secondEncoded, ok := EncodeRequest(target, decoded.Value, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无编码器", target)
			}
			second := secondEncoded.Body.MarshalCompact()

			if hashOf(first) != hashOf(second) {
				t.Fatalf("第二次编码与第一次不同 —— 编码器持隐藏状态或就地改写入参\n"+
					"第 1 次 sha256=%s\n第 2 次 sha256=%s\n%s",
					hashOf(first), hashOf(second), byteDiff(first, second))
			}
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("没有跑到任何转换用例")
	}
}
