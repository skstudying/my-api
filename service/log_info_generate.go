package service

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func appendRequestPath(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if other == nil {
		return
	}
	if ctx != nil && ctx.Request != nil && ctx.Request.URL != nil {
		if path := ctx.Request.URL.Path; path != "" {
			other["request_path"] = path
			return
		}
	}
	if relayInfo != nil && relayInfo.RequestURLPath != "" {
		path := relayInfo.RequestURLPath
		if idx := strings.Index(path, "?"); idx != -1 {
			path = path[:idx]
		}
		other["request_path"] = path
	}
}

func GenerateTextOtherInfo(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, modelRatio, groupRatio, completionRatio float64,
	cacheTokens int, cacheRatio float64, modelPrice float64, userGroupRatio float64) map[string]interface{} {
	other := make(map[string]interface{})
	other["model_ratio"] = modelRatio
	other["group_ratio"] = groupRatio
	other["completion_ratio"] = completionRatio
	other["cache_tokens"] = cacheTokens
	other["cache_ratio"] = cacheRatio
	other["model_price"] = modelPrice
	other["user_group_ratio"] = userGroupRatio
	other["frt"] = float64(relayInfo.FirstResponseTime.UnixMilli() - relayInfo.StartTime.UnixMilli())
	if relayInfo.ReasoningEffort != "" {
		other["reasoning_effort"] = relayInfo.ReasoningEffort
	}
	if relayInfo.IsModelMapped {
		other["is_model_mapped"] = true
		other["upstream_model_name"] = relayInfo.UpstreamModelName
	}

	isSystemPromptOverwritten := common.GetContextKeyBool(ctx, constant.ContextKeySystemPromptOverride)
	if isSystemPromptOverwritten {
		other["is_system_prompt_overwritten"] = true
	}

	adminInfo := make(map[string]interface{})
	adminInfo["use_channel"] = ctx.GetStringSlice("use_channel")
	isMultiKey := common.GetContextKeyBool(ctx, constant.ContextKeyChannelIsMultiKey)
	if isMultiKey {
		adminInfo["is_multi_key"] = true
		adminInfo["multi_key_index"] = common.GetContextKeyInt(ctx, constant.ContextKeyChannelMultiKeyIndex)
	}

	isLocalCountTokens := common.GetContextKeyBool(ctx, constant.ContextKeyLocalCountTokens)
	if isLocalCountTokens {
		adminInfo["local_count_tokens"] = isLocalCountTokens
	}

	other["admin_info"] = adminInfo
	appendRequestPath(ctx, relayInfo, other)
	return other
}

func GenerateWssOtherInfo(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, usage *dto.RealtimeUsage, modelRatio, groupRatio, completionRatio, audioRatio, audioCompletionRatio, modelPrice, userGroupRatio float64) map[string]interface{} {
	info := GenerateTextOtherInfo(ctx, relayInfo, modelRatio, groupRatio, completionRatio, 0, 0.0, modelPrice, userGroupRatio)
	info["ws"] = true
	info["audio_input"] = usage.InputTokenDetails.AudioTokens
	info["audio_output"] = usage.OutputTokenDetails.AudioTokens
	info["text_input"] = usage.InputTokenDetails.TextTokens
	info["text_output"] = usage.OutputTokenDetails.TextTokens
	info["audio_ratio"] = audioRatio
	info["audio_completion_ratio"] = audioCompletionRatio
	return info
}

func GenerateAudioOtherInfo(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, usage *dto.Usage, modelRatio, groupRatio, completionRatio, audioRatio, audioCompletionRatio, modelPrice, userGroupRatio float64) map[string]interface{} {
	info := GenerateTextOtherInfo(ctx, relayInfo, modelRatio, groupRatio, completionRatio, 0, 0.0, modelPrice, userGroupRatio)
	info["audio"] = true
	info["audio_input"] = usage.PromptTokensDetails.AudioTokens
	info["audio_output"] = usage.CompletionTokenDetails.AudioTokens
	info["text_input"] = usage.PromptTokensDetails.TextTokens
	info["text_output"] = usage.CompletionTokenDetails.TextTokens
	info["audio_ratio"] = audioRatio
	info["audio_completion_ratio"] = audioCompletionRatio
	return info
}

func GenerateClaudeOtherInfo(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, modelRatio, groupRatio, completionRatio float64,
	cacheTokens int, cacheRatio float64,
	cacheCreationTokens int, cacheCreationRatio float64,
	cacheCreationTokens5m int, cacheCreationRatio5m float64,
	cacheCreationTokens1h int, cacheCreationRatio1h float64,
	modelPrice float64, userGroupRatio float64) map[string]interface{} {
	info := GenerateTextOtherInfo(ctx, relayInfo, modelRatio, groupRatio, completionRatio, cacheTokens, cacheRatio, modelPrice, userGroupRatio)
	info["claude"] = true
	info["cache_creation_tokens"] = cacheCreationTokens
	info["cache_creation_ratio"] = cacheCreationRatio
	if cacheCreationTokens5m != 0 {
		info["cache_creation_tokens_5m"] = cacheCreationTokens5m
		info["cache_creation_ratio_5m"] = cacheCreationRatio5m
	}
	if cacheCreationTokens1h != 0 {
		info["cache_creation_tokens_1h"] = cacheCreationTokens1h
		info["cache_creation_ratio_1h"] = cacheCreationRatio1h
	}
	return info
}

// BuildUpstreamUsageInfo reconstructs the upstream usage info in the provider's native format.
// Returns nil if no meaningful usage data is available.
func BuildUpstreamUsageInfo(usage *dto.Usage, channelType int) map[string]interface{} {
	if usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0) {
		return nil
	}

	switch channelType {
	case constant.ChannelTypeAnthropic, constant.ChannelTypeAws:
		// Anthropic Claude format
		result := map[string]interface{}{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
		}
		if usage.PromptTokensDetails.CachedTokens > 0 {
			result["cache_read_input_tokens"] = usage.PromptTokensDetails.CachedTokens
		}
		if usage.PromptTokensDetails.CachedCreationTokens > 0 {
			result["cache_creation_input_tokens"] = usage.PromptTokensDetails.CachedCreationTokens
		}
		if usage.ClaudeCacheCreation5mTokens > 0 {
			if cacheCreation, ok := result["cache_creation"].(map[string]interface{}); ok {
				cacheCreation["ephemeral_5m_input_tokens"] = usage.ClaudeCacheCreation5mTokens
			} else {
				result["cache_creation"] = map[string]interface{}{
					"ephemeral_5m_input_tokens": usage.ClaudeCacheCreation5mTokens,
				}
			}
		}
		if usage.ClaudeCacheCreation1hTokens > 0 {
			if cacheCreation, ok := result["cache_creation"].(map[string]interface{}); ok {
				cacheCreation["ephemeral_1h_input_tokens"] = usage.ClaudeCacheCreation1hTokens
			} else {
				result["cache_creation"] = map[string]interface{}{
					"ephemeral_1h_input_tokens": usage.ClaudeCacheCreation1hTokens,
				}
			}
		}
		return result

	case constant.ChannelTypeGemini, constant.ChannelTypeVertexAi:
		// Google Gemini usageMetadata format
		result := map[string]interface{}{
			"promptTokenCount":     usage.PromptTokens,
			"candidatesTokenCount": usage.CompletionTokens,
			"totalTokenCount":      usage.TotalTokens,
		}
		if usage.PromptTokensDetails.CachedTokens > 0 {
			result["cachedContentTokenCount"] = usage.PromptTokensDetails.CachedTokens
		}
		if usage.CompletionTokenDetails.ReasoningTokens > 0 {
			result["thoughtsTokenCount"] = usage.CompletionTokenDetails.ReasoningTokens
		}
		if usage.PromptTokensDetails.AudioTokens > 0 || usage.PromptTokensDetails.TextTokens > 0 || usage.PromptTokensDetails.ImageTokens > 0 {
			var details []map[string]interface{}
			if usage.PromptTokensDetails.TextTokens > 0 {
				details = append(details, map[string]interface{}{"modality": "TEXT", "tokenCount": usage.PromptTokensDetails.TextTokens})
			}
			if usage.PromptTokensDetails.ImageTokens > 0 {
				details = append(details, map[string]interface{}{"modality": "IMAGE", "tokenCount": usage.PromptTokensDetails.ImageTokens})
			}
			if usage.PromptTokensDetails.AudioTokens > 0 {
				details = append(details, map[string]interface{}{"modality": "AUDIO", "tokenCount": usage.PromptTokensDetails.AudioTokens})
			}
			result["promptTokensDetails"] = details
		}
		if usage.CompletionTokenDetails.AudioTokens > 0 || usage.CompletionTokenDetails.TextTokens > 0 || usage.CompletionTokenDetails.ImageTokens > 0 {
			var details []map[string]interface{}
			if usage.CompletionTokenDetails.TextTokens > 0 {
				details = append(details, map[string]interface{}{"modality": "TEXT", "tokenCount": usage.CompletionTokenDetails.TextTokens})
			}
			if usage.CompletionTokenDetails.ImageTokens > 0 {
				details = append(details, map[string]interface{}{"modality": "IMAGE", "tokenCount": usage.CompletionTokenDetails.ImageTokens})
			}
			if usage.CompletionTokenDetails.AudioTokens > 0 {
				details = append(details, map[string]interface{}{"modality": "AUDIO", "tokenCount": usage.CompletionTokenDetails.AudioTokens})
			}
			result["candidatesTokensDetails"] = details
		}
		return result

	default:
		// OpenAI format (default for OpenAI, Azure, DeepSeek, Zhipu, Moonshot, xAI, etc.)
		result := map[string]interface{}{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
		promptDetails := make(map[string]interface{})
		if usage.PromptTokensDetails.CachedTokens > 0 {
			promptDetails["cached_tokens"] = usage.PromptTokensDetails.CachedTokens
		}
		if usage.PromptTokensDetails.AudioTokens > 0 {
			promptDetails["audio_tokens"] = usage.PromptTokensDetails.AudioTokens
		}
		if usage.PromptTokensDetails.ImageTokens > 0 {
			promptDetails["image_tokens"] = usage.PromptTokensDetails.ImageTokens
		}
		if len(promptDetails) > 0 {
			result["prompt_tokens_details"] = promptDetails
		}
		completionDetails := make(map[string]interface{})
		if usage.CompletionTokenDetails.ReasoningTokens > 0 {
			completionDetails["reasoning_tokens"] = usage.CompletionTokenDetails.ReasoningTokens
		}
		if usage.CompletionTokenDetails.AudioTokens > 0 {
			completionDetails["audio_tokens"] = usage.CompletionTokenDetails.AudioTokens
		}
		if usage.CompletionTokenDetails.ImageTokens > 0 {
			completionDetails["image_tokens"] = usage.CompletionTokenDetails.ImageTokens
		}
		if len(completionDetails) > 0 {
			result["completion_tokens_details"] = completionDetails
		}
		return result
	}
}

func GenerateMjOtherInfo(relayInfo *relaycommon.RelayInfo, priceData types.PerCallPriceData) map[string]interface{} {
	other := make(map[string]interface{})
	other["model_price"] = priceData.ModelPrice
	other["group_ratio"] = priceData.GroupRatioInfo.GroupRatio
	if priceData.GroupRatioInfo.HasSpecialRatio {
		other["user_group_ratio"] = priceData.GroupRatioInfo.GroupSpecialRatio
	}
	appendRequestPath(nil, relayInfo, other)
	return other
}
