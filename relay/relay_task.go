package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

/*
Task 任务通过平台、Action 区分任务
*/
func RelayTaskSubmit(c *gin.Context, info *relaycommon.RelayInfo) (taskErr *dto.TaskError) {
	info.InitChannelMeta(c)
	// ensure TaskRelayInfo is initialized to avoid nil dereference when accessing embedded fields
	if info.TaskRelayInfo == nil {
		info.TaskRelayInfo = &relaycommon.TaskRelayInfo{}
	}
	path := c.Request.URL.Path
	if strings.Contains(path, "/v1/videos/") && strings.HasSuffix(path, "/remix") {
		info.Action = constant.TaskActionRemix
	}
	if strings.HasSuffix(path, "/videos/edits") {
		info.Action = constant.TaskActionEdit
	}

	// 提取 remix 任务的 video_id
	if info.Action == constant.TaskActionRemix {
		videoID := c.Param("video_id")
		if strings.TrimSpace(videoID) == "" {
			return service.TaskErrorWrapperLocal(fmt.Errorf("video_id is required"), "invalid_request", http.StatusBadRequest)
		}
		info.OriginTaskID = videoID
	}

	platform := constant.TaskPlatform(c.GetString("platform"))

	// 获取原始任务信息
	if info.OriginTaskID != "" {
		originTask, exist, err := model.GetByTaskId(info.UserId, info.OriginTaskID)
		if err != nil {
			taskErr = service.TaskErrorWrapper(err, "get_origin_task_failed", http.StatusInternalServerError)
			return
		}
		if !exist {
			taskErr = service.TaskErrorWrapperLocal(errors.New("task_origin_not_exist"), "task_not_exist", http.StatusBadRequest)
			return
		}
		if info.OriginModelName == "" {
			if originTask.Properties.OriginModelName != "" {
				info.OriginModelName = originTask.Properties.OriginModelName
			} else if originTask.Properties.UpstreamModelName != "" {
				info.OriginModelName = originTask.Properties.UpstreamModelName
			} else {
				var taskData map[string]interface{}
				_ = json.Unmarshal(originTask.Data, &taskData)
				if m, ok := taskData["model"].(string); ok && m != "" {
					info.OriginModelName = m
					platform = originTask.Platform
				}
			}
		}
		if originTask.ChannelId != info.ChannelId {
			channel, err := model.GetChannelById(originTask.ChannelId, true)
			if err != nil {
				taskErr = service.TaskErrorWrapperLocal(err, "channel_not_found", http.StatusBadRequest)
				return
			}
			if channel.Status != common.ChannelStatusEnabled {
				taskErr = service.TaskErrorWrapperLocal(errors.New("the channel of the origin task is disabled"), "task_channel_disable", http.StatusBadRequest)
				return
			}
			key, _, newAPIError := channel.GetNextEnabledKey()
			if newAPIError != nil {
				taskErr = service.TaskErrorWrapper(newAPIError, "channel_no_available_key", newAPIError.StatusCode)
				return
			}
			common.SetContextKey(c, constant.ContextKeyChannelKey, key)
			common.SetContextKey(c, constant.ContextKeyChannelType, channel.Type)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, channel.GetBaseURL())
			common.SetContextKey(c, constant.ContextKeyChannelId, originTask.ChannelId)

			info.ChannelBaseUrl = channel.GetBaseURL()
			info.ChannelId = originTask.ChannelId
			info.ChannelType = channel.Type
			info.ApiKey = key
			platform = originTask.Platform
		}

		// 使用原始任务的参数
		if info.Action == constant.TaskActionRemix {
			var taskData map[string]interface{}
			_ = json.Unmarshal(originTask.Data, &taskData)
			secondsStr, _ := taskData["seconds"].(string)
			seconds, _ := strconv.Atoi(secondsStr)
			if seconds <= 0 {
				seconds = 4
			}
			sizeStr, _ := taskData["size"].(string)
			if info.PriceData.OtherRatios == nil {
				info.PriceData.OtherRatios = map[string]float64{}
			}
			info.PriceData.OtherRatios["seconds"] = float64(seconds)
			info.PriceData.OtherRatios["size"] = 1
			if sizeStr == "1792x1024" || sizeStr == "1024x1792" {
				info.PriceData.OtherRatios["size"] = 1.666667
			}
		}
	}
	if platform == "" {
		platform = GetTaskPlatform(c)
	}

	info.InitChannelMeta(c)
	adaptor := GetTaskAdaptor(platform)
	if adaptor == nil {
		return service.TaskErrorWrapperLocal(fmt.Errorf("invalid api platform: %s", platform), "invalid_api_platform", http.StatusBadRequest)
	}
	adaptor.Init(info)

	// 处理模型映射（渠道模型重定向）
	if err := applyTaskModelMapping(c, info); err != nil {
		return service.TaskErrorWrapperLocal(err, "model_mapping_failed", http.StatusBadRequest)
	}

	// get & validate taskRequest 获取并验证文本请求
	taskErr = adaptor.ValidateRequestAndSetAction(c, info)
	if taskErr != nil {
		return
	}

	modelName := info.OriginModelName
	if modelName == "" {
		modelName = service.CoverTaskActionToModelName(platform, info.Action)
	}
	modelPrice, success := ratio_setting.GetModelPrice(modelName, true)
	if !success {
		defaultPrice, ok := ratio_setting.GetDefaultModelPriceMap()[modelName]
		if !ok {
			modelPrice = 0.1
		} else {
			modelPrice = defaultPrice
		}
	}

	// 处理 auto 分组：从 context 获取实际选中的分组
	// 当使用 auto 分组时，Distribute 中间件会将实际选中的分组存储在 ContextKeyAutoGroup 中
	if autoGroup, exists := common.GetContextKey(c, constant.ContextKeyAutoGroup); exists {
		if groupStr, ok := autoGroup.(string); ok && groupStr != "" {
			info.UsingGroup = groupStr
		}
	}

	// 预扣
	groupRatio := ratio_setting.GetGroupRatio(info.UsingGroup)
	var ratio float64
	userGroupRatio, hasUserGroupRatio := ratio_setting.GetGroupGroupRatio(info.UserGroup, info.UsingGroup)
	if hasUserGroupRatio {
		ratio = modelPrice * userGroupRatio
	} else {
		ratio = modelPrice * groupRatio
	}
	// FIXME: 临时修补，支持任务仅按次计费
	if !common.StringsContains(constant.TaskPricePatches, modelName) {
		if len(info.PriceData.OtherRatios) > 0 {
			for _, ra := range info.PriceData.OtherRatios {
				if 1.0 != ra {
					ratio *= ra
				}
			}
		}
	}
	println(fmt.Sprintf("model: %s, model_price: %.4f, group: %s, group_ratio: %.4f, final_ratio: %.4f", modelName, modelPrice, info.UsingGroup, groupRatio, ratio))
	userQuota, err := model.GetUserQuota(info.UserId, false)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
		return
	}
	quota := int(ratio * common.QuotaPerUnit)

	// xAI input image billing (additive, set by xAI video task adaptor)
	xaiInputImageCount := c.GetInt("xai_input_image_count")
	xaiInputImagePrice := c.GetFloat64("xai_input_image_price")
	if xaiInputImageCount > 0 && xaiInputImagePrice > 0 {
		effectiveGroupRatio := groupRatio
		if hasUserGroupRatio {
			effectiveGroupRatio = userGroupRatio
		}
		xaiInputImageQuota := int(xaiInputImagePrice * float64(xaiInputImageCount) * effectiveGroupRatio * common.QuotaPerUnit)
		quota += xaiInputImageQuota
	}

	// xAI input video billing (additive, set by xAI video task adaptor for video edits)
	xaiInputVideoSeconds := c.GetFloat64("xai_input_video_seconds")
	xaiInputVideoPrice := c.GetFloat64("xai_input_video_price")
	if xaiInputVideoSeconds > 0 && xaiInputVideoPrice > 0 {
		effectiveGroupRatio := groupRatio
		if hasUserGroupRatio {
			effectiveGroupRatio = userGroupRatio
		}
		xaiInputVideoQuota := int(xaiInputVideoPrice * xaiInputVideoSeconds * effectiveGroupRatio * common.QuotaPerUnit)
		quota += xaiInputVideoQuota
	}

	if userQuota-quota < 0 {
		taskErr = service.TaskErrorWrapperLocal(errors.New("user quota is not enough"), "quota_not_enough", http.StatusForbidden)
		return
	}

	// build body
	requestBody, err := adaptor.BuildRequestBody(c, info)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "build_request_failed", http.StatusInternalServerError)
		return
	}
	// do request
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
		return
	}
	// handle response
	if resp != nil && resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		taskErr = service.TaskErrorWrapper(fmt.Errorf("%s", string(responseBody)), "fail_to_fetch_task", resp.StatusCode)
		return
	}

	defer func() {
		// release quota
		if info.ConsumeQuota && taskErr == nil {

			err := service.PostConsumeQuota(info, quota, 0, true)
			if err != nil {
				common.SysLog("error consuming token remain quota: " + err.Error())
			}
			// Video edit: defer billing log to task completion (actual duration known then)
			if quota != 0 && info.Action != constant.TaskActionEdit {
				tokenName := c.GetString("token_name")
				logContent := fmt.Sprintf("操作 %s", info.Action)
				// FIXME: 临时修补，支持任务仅按次计费
				if common.StringsContains(constant.TaskPricePatches, modelName) {
					logContent = fmt.Sprintf("%s，按次计费", logContent)
				} else {
					if len(info.PriceData.OtherRatios) > 0 {
						var contents []string
						for key, ra := range info.PriceData.OtherRatios {
							if 1.0 != ra {
								contents = append(contents, fmt.Sprintf("%s: %.2f", key, ra))
							}
						}
						if len(contents) > 0 {
							logContent = fmt.Sprintf("%s, 计算参数：%s", logContent, strings.Join(contents, ", "))
						}
					}
				}
				other := make(map[string]interface{})
				if c != nil && c.Request != nil && c.Request.URL != nil {
					other["request_path"] = c.Request.URL.Path
				}
				other["model_price"] = modelPrice
				other["group_ratio"] = groupRatio
				if hasUserGroupRatio {
					other["user_group_ratio"] = userGroupRatio
				}
				if xaiInputImageCount > 0 && xaiInputImagePrice > 0 {
					logContent = fmt.Sprintf("%s, 输入图片 %d 张 ($%.4f/张)", logContent, xaiInputImageCount, xaiInputImagePrice)
					other["xai_input_image"] = true
					other["xai_input_image_count"] = xaiInputImageCount
					other["xai_input_image_price"] = xaiInputImagePrice
				}
				model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{
					ChannelId: info.ChannelId,
					ModelName: modelName,
					TokenName: tokenName,
					Quota:     quota,
					Content:   logContent,
					TokenId:   info.TokenId,
					Group:     info.UsingGroup,
					Other:     other,
				})
				model.UpdateUserUsedQuotaAndRequestCount(info.UserId, quota)
				model.UpdateChannelUsedQuota(info.ChannelId, quota)
			}
		}
	}()

	taskID, taskData, taskErr := adaptor.DoResponse(c, resp, info)
	if taskErr != nil {
		return
	}
	info.ConsumeQuota = true
	// insert task
	task := model.InitTask(platform, info)
	task.TaskID = taskID
	task.Status = model.TaskStatusSubmitted
	task.Quota = quota
	task.Data = taskData
	task.Action = info.Action
	if info.Action == constant.TaskActionEdit {
		task.PrivateData.TokenId = info.TokenId
		task.PrivateData.TokenKey = info.TokenKey
		task.PrivateData.TokenName = c.GetString("token_name")
	}
	err = task.Insert()
	if err != nil {
		taskErr = service.TaskErrorWrapper(err, "insert_task_failed", http.StatusInternalServerError)
		return
	}
	return nil
}

var fetchRespBuilders = map[int]func(c *gin.Context) (respBody []byte, taskResp *dto.TaskError){
	relayconstant.RelayModeSunoFetchByID:  sunoFetchByIDRespBodyBuilder,
	relayconstant.RelayModeSunoFetch:      sunoFetchRespBodyBuilder,
	relayconstant.RelayModeVideoFetchByID: videoFetchByIDRespBodyBuilder,
}

func RelayTaskFetch(c *gin.Context, relayMode int) (taskResp *dto.TaskError) {
	respBuilder, ok := fetchRespBuilders[relayMode]
	if !ok {
		taskResp = service.TaskErrorWrapperLocal(errors.New("invalid_relay_mode"), "invalid_relay_mode", http.StatusBadRequest)
	}

	respBody, taskErr := respBuilder(c)
	if taskErr != nil {
		return taskErr
	}
	if len(respBody) == 0 {
		respBody = []byte("{\"code\":\"success\",\"data\":null}")
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	_, err := io.Copy(c.Writer, bytes.NewBuffer(respBody))
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError)
		return
	}
	return
}

func sunoFetchRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	userId := c.GetInt("id")
	var condition = struct {
		IDs    []any  `json:"ids"`
		Action string `json:"action"`
	}{}
	err := c.BindJSON(&condition)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "invalid_request", http.StatusBadRequest)
		return
	}
	var tasks []any
	if len(condition.IDs) > 0 {
		taskModels, err := model.GetByTaskIds(userId, condition.IDs)
		if err != nil {
			taskResp = service.TaskErrorWrapper(err, "get_tasks_failed", http.StatusInternalServerError)
			return
		}
		for _, task := range taskModels {
			tasks = append(tasks, TaskModel2Dto(task))
		}
	} else {
		tasks = make([]any, 0)
	}
	respBody, err = json.Marshal(dto.TaskResponse[[]any]{
		Code: "success",
		Data: tasks,
	})
	return
}

func sunoFetchByIDRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	taskId := c.Param("id")
	userId := c.GetInt("id")

	originTask, exist, err := model.GetByTaskId(userId, taskId)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "get_task_failed", http.StatusInternalServerError)
		return
	}
	if !exist {
		taskResp = service.TaskErrorWrapperLocal(errors.New("task_not_exist"), "task_not_exist", http.StatusBadRequest)
		return
	}

	respBody, err = json.Marshal(dto.TaskResponse[any]{
		Code: "success",
		Data: TaskModel2Dto(originTask),
	})
	return
}

func videoFetchByIDRespBodyBuilder(c *gin.Context) (respBody []byte, taskResp *dto.TaskError) {
	taskId := c.Param("task_id")
	if taskId == "" {
		taskId = c.GetString("task_id")
	}
	userId := c.GetInt("id")

	originTask, exist, err := model.GetByTaskId(userId, taskId)
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "get_task_failed", http.StatusInternalServerError)
		return
	}
	if !exist {
		taskResp = service.TaskErrorWrapperLocal(errors.New("task_not_exist"), "task_not_exist", http.StatusBadRequest)
		return
	}

	func() {
		channelModel, err2 := model.GetChannelById(originTask.ChannelId, true)
		if err2 != nil {
			common.SysLog(fmt.Sprintf("[video-poll] GetChannelById(%d) failed: %v", originTask.ChannelId, err2))
			return
		}
		if channelModel.Type != constant.ChannelTypeVertexAi && channelModel.Type != constant.ChannelTypeGemini && channelModel.Type != constant.ChannelTypeXai {
			return
		}
		baseURL := constant.ChannelBaseURLs[channelModel.Type]
		if channelModel.GetBaseURL() != "" {
			baseURL = channelModel.GetBaseURL()
		}
		proxy := channelModel.GetSetting().Proxy
		adaptor := GetTaskAdaptor(constant.TaskPlatform(strconv.Itoa(channelModel.Type)))
		if adaptor == nil {
			common.SysLog(fmt.Sprintf("[video-poll] GetTaskAdaptor(%d) returned nil", channelModel.Type))
			return
		}
		resp, err2 := adaptor.FetchTask(baseURL, channelModel.Key, map[string]any{
			"task_id": originTask.TaskID,
			"action":  originTask.Action,
		}, proxy)
		if err2 != nil {
			common.SysLog(fmt.Sprintf("[video-poll] FetchTask(%s) failed: %v", originTask.TaskID, err2))
			return
		}
		if resp == nil {
			return
		}
		defer resp.Body.Close()
		body, err2 := io.ReadAll(resp.Body)
		if err2 != nil {
			return
		}
		common.SysLog(fmt.Sprintf("[video-poll] task=%s resp=%s", originTask.TaskID, string(body)))
		ti, err2 := adaptor.ParseTaskResult(body)
		if err2 == nil && ti != nil {
			prevStatus := originTask.Status
			if ti.Status != "" {
				originTask.Status = model.TaskStatus(ti.Status)
			}
			if ti.Progress != "" {
				originTask.Progress = ti.Progress
			}
			if ti.Url != "" && !strings.HasPrefix(ti.Url, "data:") {
				originTask.FailReason = ti.Url
			}
			if ti.Reason != "" {
				originTask.FailReason = ti.Reason
			}

			statusChanged := string(prevStatus) != string(originTask.Status)

			// On success: store poll response as task data (contains duration etc.)
			if statusChanged && originTask.Status == model.TaskStatusSuccess {
				originTask.Data = body
			}

			// xAI video edit: deferred billing on success (write real cost instead of pre-charge)
			if statusChanged && originTask.Status == model.TaskStatusSuccess &&
				originTask.Action == constant.TaskActionEdit &&
				ti.Duration > 0 && originTask.Quota > 0 {
				const maxDuration = 8.7
				actualDuration := ti.Duration
				if actualDuration > maxDuration {
					actualDuration = maxDuration
				}
				actualQuota := originTask.Quota
				if actualDuration < maxDuration {
					actualQuota = int(float64(originTask.Quota) * actualDuration / maxDuration)
					if actualQuota <= 0 {
						actualQuota = 1
					}
				}
				refundQuota := originTask.Quota - actualQuota
				if refundQuota > 0 {
					model.IncreaseUserQuota(originTask.UserId, refundQuota, false)
					if originTask.PrivateData.TokenId > 0 && originTask.PrivateData.TokenKey != "" {
						model.IncreaseTokenQuota(originTask.PrivateData.TokenId, originTask.PrivateData.TokenKey, refundQuota)
					}
				}
				originTask.Quota = actualQuota

				modelName := originTask.Properties.OriginModelName
				modelPrice, _ := ratio_setting.GetModelPrice(modelName, true)
				groupRatio := ratio_setting.GetGroupRatio(originTask.Group)
				logContent := fmt.Sprintf("操作 %s, 实际视频 %.1f 秒, 输入视频 %.1f 秒 ($0.0100/秒)",
					originTask.Action, actualDuration, actualDuration)
				other := map[string]interface{}{
					"request_path":            "/v1/videos/edits",
					"model_price":             modelPrice,
					"group_ratio":             groupRatio,
					"xai_input_video":         true,
					"xai_input_video_seconds": actualDuration,
					"xai_input_video_price":   0.01,
				}
				model.RecordConsumeLog(c, originTask.UserId, model.RecordConsumeLogParams{
					ChannelId: originTask.ChannelId,
					ModelName: modelName,
					TokenName: originTask.PrivateData.TokenName,
					Quota:     actualQuota,
					Content:   logContent,
					TokenId:   originTask.PrivateData.TokenId,
					Group:     originTask.Group,
					Other:     other,
				})
				model.UpdateUserUsedQuotaAndRequestCount(originTask.UserId, actualQuota)
				model.UpdateChannelUsedQuota(originTask.ChannelId, actualQuota)
				common.SysLog(fmt.Sprintf("[video-edit-billing] task=%s actual=%.1fs quota=%d refund=%d",
					originTask.TaskID, actualDuration, actualQuota, refundQuota))
			}

			// xAI video edit: full refund on failure
			if statusChanged && originTask.Status == model.TaskStatusFailure &&
				originTask.Action == constant.TaskActionEdit &&
				originTask.Quota > 0 {
				refundQuota := originTask.Quota
				model.IncreaseUserQuota(originTask.UserId, refundQuota, false)
				if originTask.PrivateData.TokenId > 0 && originTask.PrivateData.TokenKey != "" {
					model.IncreaseTokenQuota(originTask.PrivateData.TokenId, originTask.PrivateData.TokenKey, refundQuota)
				}
				common.SysLog(fmt.Sprintf("[video-edit-billing] task=%s failed, full refund=%d", originTask.TaskID, refundQuota))
				originTask.Quota = 0
			}

			_ = originTask.Update()
			var raw map[string]any
			_ = json.Unmarshal(body, &raw)
			format := "mp4"
			if respObj, ok := raw["response"].(map[string]any); ok {
				if vids, ok := respObj["videos"].([]any); ok && len(vids) > 0 {
					if v0, ok := vids[0].(map[string]any); ok {
						if mt, ok := v0["mimeType"].(string); ok && mt != "" {
							if strings.Contains(mt, "mp4") {
								format = "mp4"
							} else {
								format = mt
							}
						}
					}
				}
			}
			status := "processing"
			switch originTask.Status {
			case model.TaskStatusSuccess:
				status = "succeeded"
			case model.TaskStatusFailure:
				status = "failed"
			case model.TaskStatusQueued, model.TaskStatusSubmitted:
				status = "queued"
			}
			if !strings.HasPrefix(c.Request.RequestURI, "/v1/videos/") {
				out := map[string]any{
					"error":    nil,
					"format":   format,
					"metadata": nil,
					"status":   status,
					"task_id":  originTask.TaskID,
					"url":      originTask.FailReason,
				}
				respBody, _ = json.Marshal(dto.TaskResponse[any]{
					Code: "success",
					Data: out,
				})
			}
		}
	}()

	if len(respBody) != 0 {
		return
	}

	if strings.HasPrefix(c.Request.RequestURI, "/v1/videos/") {
		adaptor := GetTaskAdaptor(originTask.Platform)
		if adaptor == nil {
			taskResp = service.TaskErrorWrapperLocal(fmt.Errorf("invalid channel id: %d", originTask.ChannelId), "invalid_channel_id", http.StatusBadRequest)
			return
		}
		if converter, ok := adaptor.(channel.OpenAIVideoConverter); ok {
			openAIVideoData, err := converter.ConvertToOpenAIVideo(originTask)
			if err != nil {
				taskResp = service.TaskErrorWrapper(err, "convert_to_openai_video_failed", http.StatusInternalServerError)
				return
			}
			respBody = openAIVideoData
			return
		}
		taskResp = service.TaskErrorWrapperLocal(errors.New(fmt.Sprintf("not_implemented:%s", originTask.Platform)), "not_implemented", http.StatusNotImplemented)
		return
	}
	respBody, err = json.Marshal(dto.TaskResponse[any]{
		Code: "success",
		Data: TaskModel2Dto(originTask),
	})
	if err != nil {
		taskResp = service.TaskErrorWrapper(err, "marshal_response_failed", http.StatusInternalServerError)
	}
	return
}

func TaskModel2Dto(task *model.Task) *dto.TaskDto {
	return &dto.TaskDto{
		TaskID:     task.TaskID,
		Action:     task.Action,
		Status:     string(task.Status),
		FailReason: task.FailReason,
		SubmitTime: task.SubmitTime,
		StartTime:  task.StartTime,
		FinishTime: task.FinishTime,
		Progress:   task.Progress,
		Data:       task.Data,
	}
}

// applyTaskModelMapping 处理任务请求的模型映射
// 从渠道配置的 model_mapping 中获取映射关系，将原始模型名映射到上游模型名
func applyTaskModelMapping(c *gin.Context, info *relaycommon.RelayInfo) error {
	modelMapping := c.GetString("model_mapping")
	if modelMapping == "" || modelMapping == "{}" {
		return nil
	}

	modelMap := make(map[string]string)
	if err := json.Unmarshal([]byte(modelMapping), &modelMap); err != nil {
		return fmt.Errorf("unmarshal model mapping failed: %w", err)
	}

	// 支持链式模型重定向，最终使用链尾的模型
	currentModel := info.OriginModelName
	visitedModels := map[string]bool{
		currentModel: true,
	}

	for {
		if mappedModel, exists := modelMap[currentModel]; exists && mappedModel != "" {
			// 模型重定向循环检测，避免无限循环
			if visitedModels[mappedModel] {
				if mappedModel == currentModel {
					// 自映射，如果是原始模型则不做处理
					if currentModel == info.OriginModelName {
						info.IsModelMapped = false
						return nil
					}
					// 否则已经映射过了
					info.IsModelMapped = true
					break
				}
				return fmt.Errorf("model mapping contains cycle: %s -> %s", currentModel, mappedModel)
			}
			visitedModels[mappedModel] = true
			currentModel = mappedModel
			info.IsModelMapped = true
		} else {
			break
		}
	}

	if info.IsModelMapped {
		info.UpstreamModelName = currentModel
		common.SysLog(fmt.Sprintf("Task model mapping: %s -> %s", info.OriginModelName, info.UpstreamModelName))
	}

	return nil
}
