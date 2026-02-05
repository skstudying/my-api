package volcvideo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
)

// ============================
// Request / Response structures (Volc Ark Video)
// ============================

type contentImageURL struct {
	URL string `json:"url"`
}

type contentItem struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *contentImageURL `json:"image_url,omitempty"`
	Role     string           `json:"role,omitempty"`
}

// submitRequest 火山视频生成请求结构
// 支持文生视频、首帧生视频、首尾帧生视频
type submitRequest struct {
	Model   string        `json:"model"`
	Content []contentItem `json:"content"`

	// 回调和尾帧
	CallbackURL     string `json:"callback_url,omitempty"`
	ReturnLastFrame *bool  `json:"return_last_frame,omitempty"`

	// 视频格式参数
	Resolution string `json:"resolution,omitempty"` // 480p, 720p, 1080p
	Ratio      string `json:"ratio,omitempty"`      // 16:9, 4:3, 1:1, 3:4, 9:16, 21:9, adaptive
	Duration   *int   `json:"duration,omitempty"`   // 2-12秒，Seedance 1.5 支持 [4,12] 或 -1
	Frames     *int   `json:"frames,omitempty"`     // 帧数 [29, 289]，优先级高于 duration

	// 生成控制参数
	Seed        *int  `json:"seed,omitempty"`         // [-1, 2^32-1]，-1 表示随机
	CameraFixed *bool `json:"camera_fixed,omitempty"` // 是否固定摄像头
	Watermark   *bool `json:"watermark,omitempty"`    // 是否包含水印，默认 false

	// Seedance 1.5 pro 专属参数
	GenerateAudio *bool `json:"generate_audio,omitempty"` // 是否生成同步音频，默认 false
	Draft         *bool `json:"draft,omitempty"`          // 是否开启样片模式

	// 服务参数
	ServiceTier           string `json:"service_tier,omitempty"`            // default, flex
	ExecutionExpiresAfter *int   `json:"execution_expires_after,omitempty"` // 超时时间（秒），默认 172800
}

type submitResponse struct {
	ID    string `json:"id"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type fetchResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Status  string `json:"status"`
	Content struct {
		VideoURL     string `json:"video_url"`
		LastFrameURL string `json:"last_frame_url,omitempty"`
	} `json:"content"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	Seed            int    `json:"seed"`
	Resolution      string `json:"resolution"`
	Duration        int    `json:"duration"`
	Ratio           string `json:"ratio"`
	FramesPerSecond int    `json:"framespersecond"`
	Error           struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
	} `json:"error,omitempty"`
}

// volcVideoRequest 用于解析客户端请求的扩展结构
type volcVideoRequest struct {
	relaycommon.TaskSubmitReq

	// 视频格式参数（支持直接传递或通过 metadata 传递）
	Resolution string `json:"resolution,omitempty"`
	Ratio      string `json:"ratio,omitempty"`
	Duration   *int   `json:"duration,omitempty"`
	Frames     *int   `json:"frames,omitempty"`

	// 生成控制参数
	Seed        *int  `json:"seed,omitempty"`
	CameraFixed *bool `json:"camera_fixed,omitempty"`
	Watermark   *bool `json:"watermark,omitempty"`

	// Seedance 1.5 pro 专属参数
	GenerateAudio *bool `json:"generate_audio,omitempty"`
	Draft         *bool `json:"draft,omitempty"`

	// 服务参数
	ServiceTier           string `json:"service_tier,omitempty"`
	ExecutionExpiresAfter *int   `json:"execution_expires_after,omitempty"`

	// 回调和尾帧
	CallbackURL     string `json:"callback_url,omitempty"`
	ReturnLastFrame *bool  `json:"return_last_frame,omitempty"`

	// 首尾帧生视频支持
	Images []struct {
		URL  string `json:"url"`
		Role string `json:"role,omitempty"` // first_frame, last_frame
	} `json:"images,omitempty"`
}

// ============================
// Adaptor implementation
// ============================

type TaskAdaptor struct {
	ChannelType int
	baseURL     string
	apiKey      string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	info.Action = constant.TaskActionGenerate

	// 解析扩展请求
	req := volcVideoRequest{}
	if err := common.UnmarshalBodyReusable(c, &req); err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	if strings.TrimSpace(req.Model) == "" {
		return service.TaskErrorWrapperLocal(fmt.Errorf("model is required"), "invalid_request", http.StatusBadRequest)
	}
	if strings.TrimSpace(req.Prompt) == "" && req.Image == "" && len(req.Images) == 0 {
		return service.TaskErrorWrapperLocal(fmt.Errorf("prompt or image is required"), "invalid_request", http.StatusBadRequest)
	}

	c.Set("volc_video_request", req)
	return nil
}

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return fmt.Sprintf("%s/api/v3/contents/generations/tasks", a.baseURL), nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	v, ok := c.Get("volc_video_request")
	if !ok {
		return nil, fmt.Errorf("request not found in context")
	}
	req := v.(volcVideoRequest)

	// 使用映射后的模型名称
	modelName := req.Model
	if info.UpstreamModelName != "" {
		modelName = info.UpstreamModelName
	}

	body := submitRequest{
		Model: modelName,
	}

	// ========== 构建 content ==========
	// 1. 文本提示词
	if strings.TrimSpace(req.Prompt) != "" {
		body.Content = append(body.Content, contentItem{Type: "text", Text: req.Prompt})
	}

	// 2. 单个图片（首帧生视频）
	if req.Image != "" {
		role := "first_frame"
		// 从 metadata 获取 role
		if r, ok := req.Metadata["role"].(string); ok && r != "" {
			role = r
		}
		body.Content = append(body.Content, contentItem{
			Type:     "image_url",
			ImageURL: &contentImageURL{URL: req.Image},
			Role:     role,
		})
	}

	// 3. 多图片（首尾帧生视频）- 从顶层 images 字段
	for _, img := range req.Images {
		if img.URL == "" {
			continue
		}
		role := img.Role
		if role == "" {
			role = "first_frame"
		}
		body.Content = append(body.Content, contentItem{
			Type:     "image_url",
			ImageURL: &contentImageURL{URL: img.URL},
			Role:     role,
		})
	}

	// 4. 从 metadata.images 获取多图片（兼容旧方式）
	if imgs, ok := req.Metadata["images"].([]any); ok {
		for _, it := range imgs {
			m, _ := it.(map[string]any)
			u := fmt.Sprint(m["url"])
			if u == "" {
				continue
			}
			role := fmt.Sprint(m["role"])
			if role == "" {
				role = "first_frame"
			}
			body.Content = append(body.Content, contentItem{
				Type:     "image_url",
				ImageURL: &contentImageURL{URL: u},
				Role:     role,
			})
		}
	}

	// ========== 设置视频格式参数 ==========
	// Resolution
	body.Resolution = getStringParam(req.Resolution, req.Metadata, "resolution", "")

	// Ratio
	body.Ratio = getStringParam(req.Ratio, req.Metadata, "ratio", "")

	// Duration
	body.Duration = getIntPtrParam(req.Duration, req.Metadata, "duration")

	// Frames
	body.Frames = getIntPtrParam(req.Frames, req.Metadata, "frames")

	// ========== 设置生成控制参数 ==========
	// Seed
	body.Seed = getIntPtrParam(req.Seed, req.Metadata, "seed")

	// CameraFixed
	body.CameraFixed = getBoolPtrParam(req.CameraFixed, req.Metadata, "camera_fixed")

	// Watermark - 默认 false（无水印）
	watermark := getBoolPtrParam(req.Watermark, req.Metadata, "watermark")
	if watermark == nil {
		watermark = boolPtr(false) // 默认无水印
	}
	body.Watermark = watermark

	// GenerateAudio - 默认 false（不生成音频）
	generateAudio := getBoolPtrParam(req.GenerateAudio, req.Metadata, "generate_audio")
	if generateAudio == nil {
		generateAudio = boolPtr(false) // 默认不生成音频
	}
	body.GenerateAudio = generateAudio

	// Draft
	body.Draft = getBoolPtrParam(req.Draft, req.Metadata, "draft")

	// ========== 设置服务参数 ==========
	// ServiceTier
	body.ServiceTier = getStringParam(req.ServiceTier, req.Metadata, "service_tier", "")

	// ExecutionExpiresAfter
	body.ExecutionExpiresAfter = getIntPtrParam(req.ExecutionExpiresAfter, req.Metadata, "execution_expires_after")

	// ========== 设置回调参数 ==========
	// CallbackURL
	body.CallbackURL = getStringParam(req.CallbackURL, req.Metadata, "callback_url", "")

	// ReturnLastFrame
	body.ReturnLastFrame = getBoolPtrParam(req.ReturnLastFrame, req.Metadata, "return_last_frame")

	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, _ *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	_ = resp.Body.Close()

	var sr submitResponse
	if err := json.Unmarshal(responseBody, &sr); err != nil {
		return "", nil, service.TaskErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError)
	}

	// 检查错误响应
	if sr.Error != nil && sr.Error.Code != "" {
		return "", nil, service.TaskErrorWrapperLocal(
			fmt.Errorf("%s: %s", sr.Error.Code, sr.Error.Message),
			sr.Error.Code,
			http.StatusBadRequest,
		)
	}

	if sr.ID == "" {
		return "", nil, service.TaskErrorWrapperLocal(fmt.Errorf("empty task id, response: %s", string(responseBody)), "invalid_response", http.StatusInternalServerError)
	}

	c.JSON(http.StatusOK, gin.H{"task_id": sr.ID})
	return sr.ID, responseBody, nil
}

func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, _ := body["task_id"].(string)
	if taskID == "" {
		return nil, fmt.Errorf("invalid task_id")
	}
	url := fmt.Sprintf("%s/api/v3/contents/generations/tasks/%s", baseUrl, taskID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	return service.GetHttpClient().Do(req)
}

func (a *TaskAdaptor) GetModelList() []string {
	return []string{
		// Seedance 1.0 pro
		"doubao-seedance-1-0-pro-250528",
		"doubao-seedance-pro-250528",
		"bytedance-seedance-1-0-pro-250528",
		"bytedance-seedance-pro-250528",
		"doubao-seedance-pro",
		"bytedance-seedance-pro",
		// Seedance 1.0 lite
		"doubao-seedance-1-0-lite-t2v-250428",
		"doubao-seedance-1-0-lite-i2v-250428",
		"bytedance-seedance-1-0-lite-t2v-250428",
		"bytedance-seedance-1-0-lite-i2v-250428",
		"doubao-seedance-1-0-lite-t2v",
		"doubao-seedance-1-0-lite-i2v",
		"bytedance-seedance-1-0-lite-t2v",
		"bytedance-seedance-1-0-lite-i2v",
		// Seedance 1.5 pro (支持首尾帧、音频生成)
		"doubao-seedance-1-5-pro-251215",
		"bytedance-seedance-1-5-pro-251215",
		// Wan2 系列
		"wan2-1-14b-t2v-250428",
		"wan2-1-14b-i2v-250428",
		"wan2-1-14b-flf2v-250428",
	}
}

func (a *TaskAdaptor) GetChannelName() string {
	return "volcvideo"
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	var fr fetchResponse
	if err := json.Unmarshal(respBody, &fr); err != nil {
		return nil, err
	}
	res := &relaycommon.TaskInfo{}
	res.TaskID = fr.ID

	// 检查是否有错误信息（API错误响应）
	if fr.Error.Code != "" {
		res.Status = model.TaskStatusFailure
		res.Progress = "100%"
		res.Reason = fmt.Sprintf("%s: %s", fr.Error.Code, fr.Error.Message)
		return res, nil
	}

	// 检查是否有状态信息（正常任务响应）
	if fr.Status == "" {
		res.Status = model.TaskStatusUnknown
		res.Progress = "0%"
		res.Reason = "未知响应格式"
		return res, nil
	}

	switch strings.ToLower(fr.Status) {
	case "queued":
		res.Status = model.TaskStatusQueued
		res.Progress = "20%"
	case "running":
		res.Status = model.TaskStatusInProgress
		res.Progress = "60%"
	case "succeeded":
		res.Status = model.TaskStatusSuccess
		res.Progress = "100%"
		res.Url = fr.Content.VideoURL
		// 设置成功时的详细信息到 Reason 字段（JSON格式）
		successData := map[string]interface{}{
			"id":              fr.ID,
			"model":           fr.Model,
			"status":          fr.Status,
			"content":         fr.Content,
			"usage":           fr.Usage,
			"created_at":      fr.CreatedAt,
			"updated_at":      fr.UpdatedAt,
			"seed":            fr.Seed,
			"resolution":      fr.Resolution,
			"duration":        fr.Duration,
			"ratio":           fr.Ratio,
			"framespersecond": fr.FramesPerSecond,
		}
		if dataBytes, err := json.Marshal(successData); err == nil {
			res.Reason = string(dataBytes)
		}
		// 添加usage信息用于token计费
		if fr.Usage.TotalTokens > 0 {
			res.CompletionTokens = fr.Usage.CompletionTokens
			res.TotalTokens = fr.Usage.TotalTokens
		}
	case "failed":
		res.Status = model.TaskStatusFailure
		res.Progress = "100%"
		// 设置失败原因
		if fr.Error.Message != "" {
			res.Reason = fr.Error.Message
		} else {
			res.Reason = "任务执行失败"
		}
	case "expired":
		res.Status = model.TaskStatusFailure
		res.Progress = "100%"
		res.Reason = "任务超时"
	default:
		res.Status = model.TaskStatusUnknown
		res.Progress = "0%"
		res.Reason = fmt.Sprintf("未知状态: %s", fr.Status)
	}

	return res, nil
}

// ========== 辅助函数 ==========

// boolPtr 返回 bool 指针
func boolPtr(v bool) *bool {
	return &v
}

// getStringParam 获取字符串参数，优先从直接字段获取，其次从 metadata 获取
func getStringParam(direct string, metadata map[string]interface{}, key string, defaultVal string) string {
	if direct != "" {
		return direct
	}
	if metadata != nil {
		if v, ok := metadata[key].(string); ok && v != "" {
			return v
		}
	}
	return defaultVal
}

// getIntPtrParam 获取 int 指针参数
func getIntPtrParam(direct *int, metadata map[string]interface{}, key string) *int {
	if direct != nil {
		return direct
	}
	if metadata != nil {
		if v, ok := metadata[key].(float64); ok {
			intVal := int(v)
			return &intVal
		}
		if v, ok := metadata[key].(int); ok {
			return &v
		}
	}
	return nil
}

// getBoolPtrParam 获取 bool 指针参数
func getBoolPtrParam(direct *bool, metadata map[string]interface{}, key string) *bool {
	if direct != nil {
		return direct
	}
	if metadata != nil {
		if v, ok := metadata[key].(bool); ok {
			return &v
		}
	}
	return nil
}
