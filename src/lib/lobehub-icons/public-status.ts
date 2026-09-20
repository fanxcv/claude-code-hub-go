/**
 * 本地图标面（公开状态页）：`public-status/vendor-icon` 用到的 32 个品牌图标。
 *
 * 与另两个图标面分开的理由见 `@/lib/lobehub-icons/provider-types` 文件头：
 * 公开状态页只需这一小集合，不该连带加载模型厂商的整套图标。
 *
 * 新增图标时在此追加一行，勿改回根 barrel。
 */
import AzureMono from "@lobehub/icons/es/Azure/components/Mono";
import BaichuanColor from "@lobehub/icons/es/Baichuan/components/Color";
import BedrockMono from "@lobehub/icons/es/Bedrock/components/Mono";
import ClaudeColor from "@lobehub/icons/es/Claude/components/Color";
import CohereColor from "@lobehub/icons/es/Cohere/components/Color";
import DeepSeekColor from "@lobehub/icons/es/DeepSeek/components/Color";
import DoubaoColor from "@lobehub/icons/es/Doubao/components/Color";
import FireworksColor from "@lobehub/icons/es/Fireworks/components/Color";
import GeminiColor from "@lobehub/icons/es/Gemini/components/Color";
import GemmaColor from "@lobehub/icons/es/Gemma/components/Color";
import GrokMono from "@lobehub/icons/es/Grok/components/Mono";
import GroqMono from "@lobehub/icons/es/Groq/components/Mono";
import HunyuanColor from "@lobehub/icons/es/Hunyuan/components/Color";
import InternLMColor from "@lobehub/icons/es/InternLM/components/Color";
import KimiColor from "@lobehub/icons/es/Kimi/components/Color";
import MetaColor from "@lobehub/icons/es/Meta/components/Color";
import MinimaxColor from "@lobehub/icons/es/Minimax/components/Color";
import MistralColor from "@lobehub/icons/es/Mistral/components/Color";
import MoonshotMono from "@lobehub/icons/es/Moonshot/components/Mono";
import NvidiaColor from "@lobehub/icons/es/Nvidia/components/Color";
import OllamaMono from "@lobehub/icons/es/Ollama/components/Mono";
import OpenAIMono from "@lobehub/icons/es/OpenAI/components/Mono";
import OpenRouterMono from "@lobehub/icons/es/OpenRouter/components/Mono";
import PerplexityColor from "@lobehub/icons/es/Perplexity/components/Color";
import QwenColor from "@lobehub/icons/es/Qwen/components/Color";
import SenseNovaColor from "@lobehub/icons/es/SenseNova/components/Color";
import SparkColor from "@lobehub/icons/es/Spark/components/Color";
import StepfunMono from "@lobehub/icons/es/Stepfun/components/Mono";
import TogetherColor from "@lobehub/icons/es/Together/components/Color";
import WenxinColor from "@lobehub/icons/es/Wenxin/components/Color";
import YiColor from "@lobehub/icons/es/Yi/components/Color";
import ZhipuColor from "@lobehub/icons/es/Zhipu/components/Color";

export const Azure = AzureMono;
export const Baichuan = { Color: BaichuanColor };
export const Bedrock = BedrockMono;
export const Claude = { Color: ClaudeColor };
export const Cohere = { Color: CohereColor };
export const DeepSeek = { Color: DeepSeekColor };
export const Doubao = { Color: DoubaoColor };
export const Fireworks = { Color: FireworksColor };
export const Gemini = { Color: GeminiColor };
export const Gemma = { Color: GemmaColor };
export const Grok = GrokMono;
export const Groq = GroqMono;
export const Hunyuan = { Color: HunyuanColor };
export const InternLM = { Color: InternLMColor };
export const Kimi = { Color: KimiColor };
export const Meta = { Color: MetaColor };
export const Minimax = { Color: MinimaxColor };
export const Mistral = { Color: MistralColor };
export const Moonshot = MoonshotMono;
export const Nvidia = { Color: NvidiaColor };
export const Ollama = OllamaMono;
export const OpenAI = OpenAIMono;
export const OpenRouter = OpenRouterMono;
export const Perplexity = { Color: PerplexityColor };
export const Qwen = { Color: QwenColor };
export const SenseNova = { Color: SenseNovaColor };
export const Spark = { Color: SparkColor };
export const Stepfun = StepfunMono;
export const Together = { Color: TogetherColor };
export const Wenxin = { Color: WenxinColor };
export const Yi = { Color: YiColor };
export const Zhipu = { Color: ZhipuColor };
