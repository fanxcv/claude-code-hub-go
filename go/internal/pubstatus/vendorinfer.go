package pubstatus

// 本文件由 Node 的 src/lib/model-vendor/vendor-inference.ts 机械转写（生成器见提交信息）。
// 规则表逐项同序：关键词规则是「全串子串扫描、按数组序 first-match」，顺序即语义，不可重排。

import (
	"regexp"
	"sort"
	"strings"
)

var prefixVendorAlias = map[string]string{
	"anthropic":      "anthropic",
	"openai":         "openai",
	"google":         "google",
	"meta-llama":     "meta",
	"meta":           "meta",
	"llama":          "meta",
	"deepseek":       "deepseek",
	"deepseek-ai":    "deepseek",
	"qwen":           "alibaba",
	"alibaba":        "alibaba",
	"mistral":        "mistral",
	"mistralai":      "mistral",
	"xai":            "xai",
	"x-ai":           "xai",
	"cohere":         "cohere",
	"ai21":           "ai21",
	"moonshotai":     "moonshotai",
	"moonshot":       "moonshotai",
	"zhipuai":        "zhipuai",
	"z-ai":           "zhipuai",
	"thudm":          "zhipuai",
	"minimax":        "minimax",
	"minimaxai":      "minimax",
	"perplexity":     "perplexity",
	"pplx":           "perplexity",
	"stepfun":        "stepfun",
	"stepfun-ai":     "stepfun",
	"baidu":          "baidu",
	"tencent":        "tencent",
	"bytedance":      "bytedance",
	"bytedance-seed": "bytedance",
	"volcengine":     "bytedance",
	"doubao":         "bytedance",
	"seed":           "bytedance",
	"seedance":       "bytedance",
	"xiaomi":         "xiaomi",
	"xiaomimimo":     "xiaomi",
	"mimo":           "xiaomi",
	"01-ai":          "01-ai",
	"reka":           "reka",
	"nvidia":         "nvidia",
	"ibm":            "ibm",
	"ibm-granite":    "ibm",
	"liquid":         "liquid",
	"amazon":         "amazon",
	"inception":      "inception",
	"morph":          "morph",
}

var hostPrefixes = map[string]struct{}{
	"ppio":           {},
	"sophnet":        {},
	"together":       {},
	"togetherai":     {},
	"together-ai":    {},
	"fireworks":      {},
	"fireworks-ai":   {},
	"openrouter":     {},
	"deepinfra":      {},
	"novita":         {},
	"novita-ai":      {},
	"siliconflow":    {},
	"siliconflow-cn": {},
	"302ai":          {},
	"aihubmix":       {},
	"unsloth":        {},
	"bartowski":      {},
	"thebloke":       {},
	"huggingface":    {},
	"hf":             {},
	"modelscope":     {},
	"replicate":      {},
	"nebius":         {},
	"hyperbolic":     {},
	"featherless":    {},
	"parasail":       {},
	"gmicloud":       {},
	"kluster":        {},
	"lambda":         {},
	"cloudflare":     {},
	"vercel":         {},
	"portkey":        {},
	"requesty":       {},
	"nscale":         {},
	"inference-net":  {},
	"venice":         {},
	"kenari":         {},
	"qiniu-ai":       {},
	"opencode-go":    {},
	"coding":         {},
}

var lobehubBrandVendors = map[string]string{
	"alephalpha":   "alephalpha",
	"antgroup":     "antgroup",
	"arcee":        "arcee",
	"assemblyai":   "assemblyai",
	"baichuan":     "baichuan",
	"briaai":       "briaai",
	"coqui":        "coqui",
	"dbrx":         "databricks",
	"elevenlabs":   "elevenlabs",
	"essentialai":  "essentialai",
	"fishaudio":    "fishaudio",
	"hailuo":       "minimax",
	"haiper":       "haiper",
	"hedra":        "hedra",
	"ideogram":     "ideogram",
	"inflection":   "inflection",
	"internlm":     "internlm",
	"jimeng":       "bytedance",
	"kolors":       "kwaipilot",
	"kwaipilot":    "kwaipilot",
	"llava":        "llava",
	"luma":         "luma",
	"microsoft":    "microsoft",
	"midjourney":   "midjourney",
	"myshell":      "myshell",
	"nousresearch": "nousresearch",
	"novelai":      "novelai",
	"openchat":     "openchat",
	"pika":         "pika",
	"pixverse":     "pixverse",
	"reve":         "reve",
	"runway":       "runway",
	"rwkv":         "rwkv",
	"sensenova":    "sensenova",
	"skywork":      "skywork",
	"stability":    "stability",
	"suno":         "suno",
	"tiangong":     "skywork",
	"tripo":        "tripo",
	"upstage":      "upstage",
	"vidu":         "vidu",
	"xuanyuan":     "xuanyuan",
	"yandex":       "yandex",
	"yuanbao":      "tencent",
}

// keywordVendorRules 是「关键词 -> vendor」的有序规则表（同序即语义）。
var keywordVendorRules = []struct {
	re     *regexp.Regexp
	vendor string
}{
	{regexp.MustCompile("xiaomimimo|xiaomi|\\bmimo\\b"), "xiaomi"},
	{regexp.MustCompile("deepseek"), "deepseek"},
	{regexp.MustCompile("doubao|seedance|seedream|seed-oss|\\bseed-\\d|ui-tars|bytedance|volcengine"), "bytedance"},
	{regexp.MustCompile("nemotron|\\bnvidia\\b"), "nvidia"},
	{regexp.MustCompile("\\bphi-?\\d|wizardlm|\\borca-2|\\bmai-(ds|voice|\\d)"), "microsoft"},
	{regexp.MustCompile("\\byi-\\d|\\byi-(lightning|vision|large|medium|coder|spark)|\\b01-?ai|yi1\\.5"), "01-ai"},
	{regexp.MustCompile("sparkdesk|iflytek|spark-(max|lite|pro|ultra|x1)|\\bspark4"), "iflytek"},
	{regexp.MustCompile("360gpt|360zhinao"), "360"},
	{regexp.MustCompile("kimi|moonshot"), "moonshotai"},
	{regexp.MustCompile("ernie|wenxin|qianfan"), "baidu"},
	{regexp.MustCompile("hunyuan|\\bhy-?\\d|\\bhy-(mt|image|video|3d|t1|turbo|large|standard|lite|vision|role|a13b|dense|moe|code)"), "tencent"},
	{regexp.MustCompile("grok"), "xai"},
	{regexp.MustCompile("\\bcommand-?(r|a|light|nightly)|\\bcommand\\b"), "cohere"},
	{regexp.MustCompile("pixtral|codestral|ministral|magistral|devstral|mixtral|mistral"), "mistral"},
	{regexp.MustCompile("granite"), "ibm"},
	{regexp.MustCompile("\\blfm-?\\d|\\blfm\\b"), "liquid"},
	{regexp.MustCompile("jamba"), "ai21"},
	{regexp.MustCompile("stepfun|\\bstep-\\d|step-r1|step-audio"), "stepfun"},
	{regexp.MustCompile("minimax|\\babab"), "minimax"},
	{regexp.MustCompile("\\breka-|\\breka\\b"), "reka"},
	{regexp.MustCompile("sonar|perplexity"), "perplexity"},
	{regexp.MustCompile("falcon"), "tii"},
	{regexp.MustCompile("deepgram"), "deepgram"},
	{regexp.MustCompile("\\bjina"), "jina"},
	{regexp.MustCompile("voyage"), "voyage"},
	{regexp.MustCompile("\\bbge-|\\bbge\\b|baai"), "baai"},
	{regexp.MustCompile("black-forest|\\bflux-?\\d|\\bflux-(pro|dev|schnell|kontext|krea|1)|\\bflux\\b"), "bfl"},
	{regexp.MustCompile("\\bkling"), "kling"},
	{regexp.MustCompile("recraft"), "recraft"},
	{regexp.MustCompile("longcat"), "longcat"},
	{regexp.MustCompile("\\bling-(lite|plus|flash|mini|coder|omni|1t|\\d)|\\bling\\b|bailing|inclusionai|\\bring-(lite|flash|mini|1t)"), "antgroup"},
	{regexp.MustCompile("chatglm|autoglm|charglm|codegeex|cogview|cogvideo|\\bglm-?\\d|\\bglm-|\\bglm\\b|zhipu"), "zhipuai"},
	{regexp.MustCompile("qwen|qwq|qvq|tongyi|wanx|marco-o1"), "alibaba"},
	{regexp.MustCompile("gemini|gemma|\\bpalm-2|imagen|nano-banana|\\bbison\\b|\\bgecko\\b"), "google"},
	{regexp.MustCompile("codellama|llama"), "meta"},
	{regexp.MustCompile("\\bgpt-|\\bo1-|\\bo3-|\\bo4-|davinci|\\bwhisper\\b|dall-e|\\bchatgpt|text-embedding-(ada|3)"), "openai"},
	{regexp.MustCompile("claude"), "anthropic"},
	{regexp.MustCompile("facebook|\\bbart-|\\bopt-\\d|blenderbot"), "meta"},
}

// lobehubBrandRules 按 token 长度降序（最长/最具体优先）。
var lobehubBrandRules = func() [][2]string {
	rules := make([][2]string, 0, len(lobehubBrandVendors))
	for token, vendor := range lobehubBrandVendors {
		rules = append(rules, [2]string{token, vendor})
	}
	sort.Slice(rules, func(i, j int) bool { return len(rules[i][0]) > len(rules[j][0]) })
	return rules
}()

const unknownVendor = "other"

var regionPrefix = regexp.MustCompile(`^(us|eu|jp|au|apac|global|us-gov|ca|sa)\.`)

var bareDotPrefix = regexp.MustCompile(`^([a-z0-9-]+)\.`)

func stripRegionPrefix(value string) string {
	out := value
	for regionPrefix.MatchString(out) {
		out = regionPrefix.ReplaceAllString(out, "")
	}
	return out
}

func vendorOfPrefix(prefix string) (string, bool) {
	if prefix == "" {
		return "", false
	}
	v, ok := prefixVendorAlias[strings.ToLower(prefix)]
	return v, ok
}

func keywordScan(value string) (string, bool) {
	for _, rule := range keywordVendorRules {
		if rule.re.MatchString(value) {
			return rule.vendor, true
		}
	}
	return "", false
}

func lobehubBrandScan(value string) (string, bool) {
	for _, rule := range lobehubBrandRules {
		if strings.Contains(value, rule[0]) {
			return rule[1], true
		}
	}
	return "", false
}

// inferVendorFromModelName 从模型调用名尽力推断 vendor slug（照 Node 的五步）。
func inferVendorFromModelName(modelName string) string {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	if lower == "" {
		return unknownVendor
	}

	if strings.HasPrefix(lower, "@cf/") {
		parts := strings.Split(lower, "/")
		org := ""
		if len(parts) > 1 {
			org = parts[1]
		}
		if org == "facebook" {
			return "meta"
		}
		if v, ok := vendorOfPrefix(org); ok {
			return v
		}
		if v, ok := keywordScan(org); ok {
			return v
		}
		rest := ""
		if len(parts) > 2 {
			rest = strings.Join(parts[2:], "/")
		}
		if v, ok := keywordScan(rest); ok {
			return v
		}
		return unknownVendor
	}

	slash := strings.Index(lower, "/")
	if slash >= 0 {
		org := lower[:slash]
		if _, isHost := hostPrefixes[org]; !isHost {
			if v, ok := vendorOfPrefix(org); ok {
				return v
			}
			if v, ok := keywordScan(org); ok {
				return v
			}
		}
	}

	if v, ok := keywordScan(lower); ok {
		return v
	}

	bare := lower
	if slash >= 0 {
		bare = lower[slash+1:]
	}
	if m := bareDotPrefix.FindStringSubmatch(stripRegionPrefix(bare)); m != nil {
		if v, ok := vendorOfPrefix(m[1]); ok {
			return v
		}
	}
	if dash := strings.Index(bare, "-"); dash > 0 {
		if v, ok := vendorOfPrefix(bare[:dash]); ok {
			return v
		}
	}
	if v, ok := vendorOfPrefix(bare); ok {
		return v
	}

	if v, ok := lobehubBrandScan(lower); ok {
		return v
	}
	return unknownVendor
}
