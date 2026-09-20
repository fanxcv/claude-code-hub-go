/**
 * 本地图标面（供应商类型）：`provider-type-utils` 只用到 4 个图标。
 *
 * 为什么不从 `@lobehub/icons` 根 barrel 引入：包内 `es/` 约 6.9 MiB，每个图标的入口会把
 * Mono/Text/Avatar 等变体无条件挂到同一对象上（`Icons.Text = Text` 这类赋值），打包器
 * 无法逐变体裁掉。
 *
 * 为什么按消费方分成三个文件（provider-types / vendor / public-status）而不是合成一个：
 * 合成一个模块会让「只要 4 个图标」的供应商页与「要 87 个图标」的使用记录页共用同一个
 * chunk，实测这类页面各涨约 270 KiB。按消费方拆开后，每个页面的图标集合各自成 chunk。
 *
 * 新增图标或变体时在此追加一行，勿改回根 barrel。
 */
import AnthropicAvatar from "@lobehub/icons/es/Anthropic/components/Avatar";
import ClaudeColor from "@lobehub/icons/es/Claude/components/Color";
import GeminiColor from "@lobehub/icons/es/Gemini/components/Color";
import OpenAIMono from "@lobehub/icons/es/OpenAI/components/Mono";

export const Anthropic = { Avatar: AnthropicAvatar };
export const Claude = { Color: ClaudeColor };
export const Gemini = { Color: GeminiColor };
export const OpenAI = OpenAIMono;
