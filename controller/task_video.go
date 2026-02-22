package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

func UpdateVideoTaskAll(ctx context.Context, platform constant.TaskPlatform, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	for channelId, taskIds := range taskChannelM {
		if err := updateVideoTaskAll(ctx, platform, channelId, taskIds, taskM); err != nil {
			logger.LogError(ctx, fmt.Sprintf("Channel #%d failed to update video async tasks: %s", channelId, err.Error()))
		}
	}
	return nil
}

func updateVideoTaskAll(ctx context.Context, platform constant.TaskPlatform, channelId int, taskIds []string, taskM map[string]*model.Task) error {
	logger.LogInfo(ctx, fmt.Sprintf("Channel #%d pending video tasks: %d", channelId, len(taskIds)))
	if len(taskIds) == 0 {
		return nil
	}
	cacheGetChannel, err := model.CacheGetChannel(channelId)
	if err != nil {
		errUpdate := model.TaskBulkUpdate(taskIds, map[string]any{
			"fail_reason": fmt.Sprintf("Failed to get channel info, channel ID: %d", channelId),
			"status":      "FAILURE",
			"progress":    "100%",
		})
		if errUpdate != nil {
			common.SysLog(fmt.Sprintf("UpdateVideoTask error: %v", errUpdate))
		}
		return fmt.Errorf("CacheGetChannel failed: %w", err)
	}
	adaptor := relay.GetTaskAdaptor(platform)
	if adaptor == nil {
		return fmt.Errorf("video adaptor not found")
	}
	info := &relaycommon.RelayInfo{}
	info.ChannelMeta = &relaycommon.ChannelMeta{
		ChannelBaseUrl: cacheGetChannel.GetBaseURL(),
	}
	info.ApiKey = cacheGetChannel.Key
	adaptor.Init(info)
	for _, taskId := range taskIds {
		if err := updateVideoSingleTask(ctx, adaptor, cacheGetChannel, taskId, taskM); err != nil {
			logger.LogError(ctx, fmt.Sprintf("Failed to update video task %s: %s", taskId, err.Error()))
		}
	}
	return nil
}

func updateVideoSingleTask(ctx context.Context, adaptor channel.TaskAdaptor, channel *model.Channel, taskId string, taskM map[string]*model.Task) error {
	baseURL := constant.ChannelBaseURLs[channel.Type]
	if channel.GetBaseURL() != "" {
		baseURL = channel.GetBaseURL()
	}
	proxy := channel.GetSetting().Proxy

	task := taskM[taskId]
	if task == nil {
		logger.LogError(ctx, fmt.Sprintf("Task %s not found in taskM", taskId))
		return fmt.Errorf("task %s not found", taskId)
	}

	if constant.VideoTaskTimeoutMinutes > 0 && task.SubmitTime > 0 {
		elapsed := time.Now().Unix() - task.SubmitTime
		if elapsed > int64(constant.VideoTaskTimeoutMinutes*60) {
			logger.LogWarn(ctx, fmt.Sprintf("Task %s timed out after %d seconds, marking as failure", taskId, elapsed))
			preStatus := task.Status
			task.Status = model.TaskStatusFailure
			task.Progress = "100%"
			task.FinishTime = time.Now().Unix()
			task.FailReason = fmt.Sprintf("task timed out after %d minutes", constant.VideoTaskTimeoutMinutes)
			quota := task.Quota
			if quota != 0 && preStatus != model.TaskStatusFailure {
				task.Quota = 0
			}
			if err := task.Update(); err != nil {
				return fmt.Errorf("update timed out task failed: %w", err)
			}
			if quota != 0 && preStatus != model.TaskStatusFailure {
				model.IncreaseUserQuota(task.UserId, quota, false)
				if task.PrivateData.TokenId > 0 && task.PrivateData.TokenKey != "" {
					model.IncreaseTokenQuota(task.PrivateData.TokenId, task.PrivateData.TokenKey, quota)
				}
				logContent := fmt.Sprintf("Video task timed out %s, refund %s", task.TaskID, logger.LogQuota(quota))
				model.RecordLog(task.UserId, model.LogTypeSystem, logContent)
			}
			return nil
		}
	}

	key := channel.Key

	privateData := task.PrivateData
	if privateData.Key != "" {
		key = privateData.Key
	}
	resp, err := adaptor.FetchTask(baseURL, key, map[string]any{
		"task_id": taskId,
		"action":  task.Action,
	}, proxy)
	if err != nil {
		return fmt.Errorf("fetchTask failed for task %s: %w", taskId, err)
	}
	//if resp.StatusCode != http.StatusOK {
	//return fmt.Errorf("get Video Task status code: %d", resp.StatusCode)
	//}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("readAll failed for task %s: %w", taskId, err)
	}

	logger.LogDebug(ctx, fmt.Sprintf("UpdateVideoSingleTask response: %s", string(responseBody)))

	taskResult := &relaycommon.TaskInfo{}
	// try parse as New API response format
	var responseItems dto.TaskResponse[model.Task]
	if err = common.Unmarshal(responseBody, &responseItems); err == nil && responseItems.IsSuccess() {
		logger.LogDebug(ctx, fmt.Sprintf("UpdateVideoSingleTask parsed as new api response format: %+v", responseItems))
		t := responseItems.Data
		taskResult.TaskID = t.TaskID
		taskResult.Status = string(t.Status)
		taskResult.Url = t.FailReason
		taskResult.Progress = t.Progress
		taskResult.Reason = t.FailReason
		task.Data = t.Data
	} else if taskResult, err = adaptor.ParseTaskResult(responseBody); err != nil {
		return fmt.Errorf("parseTaskResult failed for task %s: %w", taskId, err)
	} else {
		task.Data = redactVideoResponseBody(responseBody)
	}

	logger.LogDebug(ctx, fmt.Sprintf("UpdateVideoSingleTask taskResult: %+v", taskResult))

	now := time.Now().Unix()
	if taskResult.Status == "" {
		//return fmt.Errorf("task %s status is empty", taskId)
		taskResult = relaycommon.FailTaskInfo("upstream returned empty status")
	}

	// 记录原本的状态，防止重复退款
	shouldRefund := false
	quota := task.Quota
	preStatus := task.Status

	task.Status = model.TaskStatus(taskResult.Status)
	switch taskResult.Status {
	case model.TaskStatusSubmitted:
		task.Progress = "10%"
	case model.TaskStatusQueued:
		task.Progress = "20%"
	case model.TaskStatusInProgress:
		task.Progress = "30%"
		if task.StartTime == 0 {
			task.StartTime = now
		}
	case model.TaskStatusSuccess:
		task.Progress = "100%"
		if task.FinishTime == 0 {
			task.FinishTime = now
		}
		// 只有非 data: URL 才设置为 FailReason
		if !(len(taskResult.Url) > 5 && taskResult.Url[:5] == "data:") {
			task.FailReason = taskResult.Url
		}

		// 如果返回了 total_tokens 并且配置了模型倍率(非固定价格),则重新计费
		if taskResult.TotalTokens > 0 {
			// 获取模型名称
			var modelName string

			// 从taskResult.Reason中解析（包含完整的响应数据）
			if taskResult.Reason != "" {
				var successData map[string]interface{}
				if err := json.Unmarshal([]byte(taskResult.Reason), &successData); err == nil {
					if model, ok := successData["model"].(string); ok {
						modelName = model
					}
				}
			}

			// 如果还是没有获取到，尝试从任务数据中获取
			if modelName == "" {
				var taskData map[string]interface{}
				if err := json.Unmarshal(task.Data, &taskData); err == nil {
					if model, ok := taskData["model"].(string); ok {
						modelName = model
					}
				}
			}

			// 如果还是没有，使用默认的模型名称
			if modelName == "" {
				modelName = "doubao-seedance-1-0-lite-i2v" // 默认模型
				logger.LogInfo(ctx, fmt.Sprintf("任务 %s 无法获取模型名称，使用默认模型: %s", task.TaskID, modelName))
			}

			// 获取模型价格和倍率
			modelRatio, hasRatioSetting, _ := ratio_setting.GetModelRatio(modelName)
			// 只有配置了倍率(非固定价格)时才按 token 重新计费
			if hasRatioSetting && modelRatio > 0 {
				// 获取用户和组的倍率信息
				group := task.Group
				if group == "" {
					user, err := model.GetUserById(task.UserId, false)
					if err == nil {
						group = user.Group
					}
				}
				if group != "" {
					groupRatio := ratio_setting.GetGroupRatio(group)
					userGroupRatio, hasUserGroupRatio := ratio_setting.GetGroupGroupRatio(group, group)

					var finalGroupRatio float64
					if hasUserGroupRatio {
						finalGroupRatio = userGroupRatio
					} else {
						finalGroupRatio = groupRatio
					}

					// 计算实际应扣费额度: totalTokens * modelRatio * groupRatio
					actualQuota := int(float64(taskResult.TotalTokens) * modelRatio * finalGroupRatio)

					// 计算差额
					preConsumedQuota := task.Quota
					quotaDelta := actualQuota - preConsumedQuota

					if quotaDelta > 0 {
						// 需要补扣费
						logger.LogInfo(ctx, fmt.Sprintf("视频任务 %s 预扣费后补扣费：%s（实际消耗：%s，预扣费：%s，tokens：%d）",
							task.TaskID,
							logger.LogQuota(quotaDelta),
							logger.LogQuota(actualQuota),
							logger.LogQuota(preConsumedQuota),
							taskResult.TotalTokens,
						))
						if err := model.DecreaseUserQuota(task.UserId, quotaDelta); err != nil {
							logger.LogError(ctx, fmt.Sprintf("补扣费失败: %s", err.Error()))
						} else {
							model.UpdateUserUsedQuotaAndRequestCount(task.UserId, quotaDelta)
							model.UpdateChannelUsedQuota(task.ChannelId, quotaDelta)
							task.Quota = actualQuota // 更新任务记录的实际扣费额度

							// 记录消费日志
							logContent := fmt.Sprintf("视频任务成功补扣费，模型倍率 %.2f，分组倍率 %.2f，tokens %d，预扣费 %s，实际扣费 %s，补扣费 %s",
								modelRatio, finalGroupRatio, taskResult.TotalTokens,
								logger.LogQuota(preConsumedQuota), logger.LogQuota(actualQuota), logger.LogQuota(quotaDelta))
							model.RecordLog(task.UserId, model.LogTypeSystem, logContent)
						}
					} else if quotaDelta < 0 {
						// 需要退还多扣的费用
						refundQuota := -quotaDelta
						logger.LogInfo(ctx, fmt.Sprintf("视频任务 %s 预扣费后返还：%s（实际消耗：%s，预扣费：%s，tokens：%d）",
							task.TaskID,
							logger.LogQuota(refundQuota),
							logger.LogQuota(actualQuota),
							logger.LogQuota(preConsumedQuota),
							taskResult.TotalTokens,
						))
						if err := model.IncreaseUserQuota(task.UserId, refundQuota, false); err != nil {
							logger.LogError(ctx, fmt.Sprintf("退还预扣费失败: %s", err.Error()))
						} else {
							task.Quota = actualQuota // 更新任务记录的实际扣费额度

							// 记录退款日志
							logContent := fmt.Sprintf("视频任务成功退还多扣费用，模型倍率 %.2f，分组倍率 %.2f，tokens %d，预扣费 %s，实际扣费 %s，退还 %s",
								modelRatio, finalGroupRatio, taskResult.TotalTokens,
								logger.LogQuota(preConsumedQuota), logger.LogQuota(actualQuota), logger.LogQuota(refundQuota))
							model.RecordLog(task.UserId, model.LogTypeSystem, logContent)
						}
					} else {
						// quotaDelta == 0, 预扣费刚好准确
						logger.LogInfo(ctx, fmt.Sprintf("视频任务 %s 预扣费准确（%s，tokens：%d）",
							task.TaskID, logger.LogQuota(actualQuota), taskResult.TotalTokens))
					}
				}
			}
		}
		// xAI video edit: deferred billing with actual duration
		if string(preStatus) != string(model.TaskStatusSuccess) &&
			task.Action == constant.TaskActionEdit &&
			taskResult.Duration > 0 && task.Quota > 0 {
			const maxDuration = 8.7
			actualDuration := taskResult.Duration
			if actualDuration > maxDuration {
				actualDuration = maxDuration
			}
			actualQuota := task.Quota
			if actualDuration < maxDuration {
				actualQuota = int(float64(task.Quota) * actualDuration / maxDuration)
				if actualQuota <= 0 {
					actualQuota = 1
				}
			}
			refundQuota := task.Quota - actualQuota
			if refundQuota > 0 {
				model.IncreaseUserQuota(task.UserId, refundQuota, false)
				if task.PrivateData.TokenId > 0 && task.PrivateData.TokenKey != "" {
					model.IncreaseTokenQuota(task.PrivateData.TokenId, task.PrivateData.TokenKey, refundQuota)
				}
			}
			task.Quota = actualQuota

			modelName := task.Properties.OriginModelName
			modelPrice, _ := ratio_setting.GetModelPrice(modelName, true)
			groupRatio := ratio_setting.GetGroupRatio(task.Group)
			logContent := fmt.Sprintf("操作 %s, 实际视频 %.1f 秒, 输入视频 %.1f 秒 ($0.0100/秒)",
				task.Action, actualDuration, actualDuration)
			other := map[string]interface{}{
				"request_path":            "/v1/videos/edits",
				"model_price":             modelPrice,
				"group_ratio":             groupRatio,
				"task_id":                 task.TaskID,
				"xai_input_video":         true,
				"xai_input_video_seconds": actualDuration,
				"xai_input_video_price":   0.01,
			}
			model.RecordConsumeLog(nil, task.UserId, model.RecordConsumeLogParams{
				ChannelId: task.ChannelId,
				ModelName: modelName,
				TokenName: task.PrivateData.TokenName,
				Quota:     actualQuota,
				Content:   logContent,
				TokenId:   task.PrivateData.TokenId,
				Group:     task.Group,
				Other:     other,
			})
			model.UpdateUserUsedQuotaAndRequestCount(task.UserId, actualQuota)
			model.UpdateChannelUsedQuota(task.ChannelId, actualQuota)
			logger.LogInfo(ctx, fmt.Sprintf("[video-edit-billing] task=%s actual=%.1fs quota=%d refund=%d",
				task.TaskID, actualDuration, actualQuota, refundQuota))
		}

	case model.TaskStatusFailure:
		logger.LogJson(ctx, fmt.Sprintf("Task %s failed", taskId), task)
		task.Status = model.TaskStatusFailure
		task.Progress = "100%"
		if task.FinishTime == 0 {
			task.FinishTime = now
		}
		task.FailReason = taskResult.Reason
		logger.LogInfo(ctx, fmt.Sprintf("Task %s failed: %s", task.TaskID, task.FailReason))
		taskResult.Progress = "100%"

		isModeration := strings.Contains(strings.ToLower(taskResult.Reason), "content moderation")

		if isModeration && quota != 0 && preStatus != model.TaskStatusFailure {
			if task.Action == constant.TaskActionEdit {
				// Edit moderation: xAI charges full amount. Bill at 8 seconds (output + input).
				moderationDuration := 8.0
				moderationQuota := int(float64(task.Quota) * moderationDuration / 8.7)
				if moderationQuota <= 0 {
					moderationQuota = 1
				}
				refundDiff := task.Quota - moderationQuota
				if refundDiff > 0 {
					model.IncreaseUserQuota(task.UserId, refundDiff, false)
					if task.PrivateData.TokenId > 0 && task.PrivateData.TokenKey != "" {
						model.IncreaseTokenQuota(task.PrivateData.TokenId, task.PrivateData.TokenKey, refundDiff)
					}
				}
				task.Quota = moderationQuota

				modelName := task.Properties.OriginModelName
				modelPrice, _ := ratio_setting.GetModelPrice(modelName, true)
				groupRatio := ratio_setting.GetGroupRatio(task.Group)
				logContent := fmt.Sprintf("操作 %s (内容审核扣费), 视频 %.1f 秒, 输入视频 %.1f 秒 ($0.0100/秒)",
					task.Action, moderationDuration, moderationDuration)
				other := map[string]interface{}{
					"request_path":            "/v1/videos/edits",
					"model_price":             modelPrice,
					"group_ratio":             groupRatio,
					"task_id":                 task.TaskID,
					"moderation":              true,
					"xai_input_video":         true,
					"xai_input_video_seconds": moderationDuration,
					"xai_input_video_price":   0.01,
				}
				model.RecordConsumeLog(nil, task.UserId, model.RecordConsumeLogParams{
					ChannelId: task.ChannelId,
					ModelName: modelName,
					TokenName: task.PrivateData.TokenName,
					Quota:     moderationQuota,
					Content:   logContent,
					TokenId:   task.PrivateData.TokenId,
					Group:     task.Group,
					Other:     other,
				})
				model.UpdateUserUsedQuotaAndRequestCount(task.UserId, moderationQuota)
				model.UpdateChannelUsedQuota(task.ChannelId, moderationQuota)
				logger.LogInfo(ctx, fmt.Sprintf("[video-edit-moderation] task=%s duration=%.1fs quota=%d refund_diff=%d",
					task.TaskID, moderationDuration, moderationQuota, refundDiff))
			} else {
				// Non-edit moderation: charge already logged at submission, don't refund.
				// Record a zero-quota informational consumption log.
				modelName := task.Properties.OriginModelName
				logContent := fmt.Sprintf("任务违规(content moderation)，费用不予返还，原始扣费 %s", logger.LogQuota(quota))
				other := map[string]interface{}{
					"moderation": true,
					"task_id":    task.TaskID,
				}
				model.RecordConsumeLog(nil, task.UserId, model.RecordConsumeLogParams{
					ChannelId: task.ChannelId,
					ModelName: modelName,
					TokenName: task.PrivateData.TokenName,
					Quota:     0,
					Content:   logContent,
					TokenId:   task.PrivateData.TokenId,
					Group:     task.Group,
					Other:     other,
				})
				logger.LogInfo(ctx, fmt.Sprintf("[video-moderation] task=%s kept charge, quota=%d", task.TaskID, task.Quota))
			}
		} else if !isModeration && quota != 0 {
			if preStatus != model.TaskStatusFailure {
				shouldRefund = true
				task.Quota = 0
			} else {
				logger.LogWarn(ctx, fmt.Sprintf("Task %s already in failure status, skip refund", task.TaskID))
			}
		}
	default:
		return fmt.Errorf("unknown task status %s for task %s", taskResult.Status, taskId)
	}
	if taskResult.Progress != "" {
		task.Progress = taskResult.Progress
	}
	if err := task.Update(); err != nil {
		common.SysLog("UpdateVideoTask task error: " + err.Error())
		shouldRefund = false
	}

	if shouldRefund {
		if err := model.IncreaseUserQuota(task.UserId, quota, false); err != nil {
			logger.LogWarn(ctx, "Failed to increase user quota: "+err.Error())
		}
		if task.PrivateData.TokenId > 0 && task.PrivateData.TokenKey != "" {
			model.IncreaseTokenQuota(task.PrivateData.TokenId, task.PrivateData.TokenKey, quota)
		}
		logContent := fmt.Sprintf("Video async task failed %s, refund %s", task.TaskID, logger.LogQuota(quota))
		model.RecordLog(task.UserId, model.LogTypeSystem, logContent)
	}

	return nil
}

func redactVideoResponseBody(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	resp, _ := m["response"].(map[string]any)
	if resp != nil {
		delete(resp, "bytesBase64Encoded")
		if v, ok := resp["video"].(string); ok {
			resp["video"] = truncateBase64(v)
		}
		if vs, ok := resp["videos"].([]any); ok {
			for i := range vs {
				if vm, ok := vs[i].(map[string]any); ok {
					delete(vm, "bytesBase64Encoded")
				}
			}
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

func truncateBase64(s string) string {
	const maxKeep = 256
	if len(s) <= maxKeep {
		return s
	}
	return s[:maxKeep] + "..."
}
