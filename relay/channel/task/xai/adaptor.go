package xai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

type submitResponse struct {
	RequestID string `json:"request_id"`
}

type pollResponse struct {
	Status string     `json:"status"` // pending, done, expired
	Video  *videoData `json:"video,omitempty"`
	Model  string     `json:"model,omitempty"`
}

type videoData struct {
	URL               string  `json:"url"`
	Duration          float64 `json:"duration"`
	RespectModeration bool    `json:"respect_moderation"`
}

type TaskAdaptor struct {
	apiKey  string
	baseURL string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *dto.TaskError {
	if taskErr := relaycommon.ValidateMultipartDirect(c, info); taskErr != nil {
		return taskErr
	}

	var req struct {
		Duration    int             `json:"duration"`
		Seconds     string          `json:"seconds"`
		AspectRatio string          `json:"aspect_ratio"`
		Resolution  string          `json:"resolution"`
		Image       json.RawMessage `json:"image"`
	}
	_ = common.UnmarshalBodyReusable(c, &req)

	seconds := req.Duration
	if seconds == 0 {
		s, _ := strconv.Atoi(req.Seconds)
		seconds = s
	}
	if seconds <= 0 {
		seconds = 5
	}

	info.PriceData.OtherRatios = map[string]float64{
		"seconds": float64(seconds),
	}
	if req.Resolution == "720p" {
		info.PriceData.OtherRatios["resolution(720p)"] = 1.4 // $0.07 / $0.05 = 1.4
	}

	// grok-imagine-video input image billing: $0.002 per input image
	if len(req.Image) > 0 {
		c.Set("xai_input_image_count", 1)
		c.Set("xai_input_image_price", 0.002)
	}

	return nil
}

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return fmt.Sprintf("%s/v1/videos/generations", a.baseURL), nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	cachedBody, err := common.GetRequestBody(c)
	if err != nil {
		return nil, fmt.Errorf("get request body failed: %w", err)
	}
	return bytes.NewReader(cachedBody), nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (taskID string, taskData []byte, taskErr *dto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	_ = resp.Body.Close()

	var sResp submitResponse
	if err := common.Unmarshal(responseBody, &sResp); err != nil {
		return "", nil, service.TaskErrorWrapper(fmt.Errorf("body: %s, err: %w", responseBody, err), "unmarshal_response_body_failed", http.StatusInternalServerError)
	}

	if sResp.RequestID == "" {
		return "", nil, service.TaskErrorWrapperLocal(fmt.Errorf("request_id is empty, body: %s", responseBody), "invalid_response", http.StatusInternalServerError)
	}

	ov := dto.NewOpenAIVideo()
	ov.ID = sResp.RequestID
	ov.Status = dto.VideoStatusQueued
	if info.UpstreamModelName != "" {
		ov.Model = info.UpstreamModelName
	} else {
		ov.Model = info.OriginModelName
	}
	c.JSON(http.StatusOK, ov)

	return sResp.RequestID, responseBody, nil
}

func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok {
		return nil, fmt.Errorf("invalid task_id")
	}

	// Handle multi-key channels: use only the first key
	key = strings.TrimSpace(strings.SplitN(key, "\n", 2)[0])

	uri := fmt.Sprintf("%s/v1/videos/%s", baseUrl, taskID)
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("xAI video poll returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return resp, nil
}

func (a *TaskAdaptor) GetModelList() []string {
	return ModelList
}

func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	var pResp pollResponse
	if err := common.Unmarshal(respBody, &pResp); err != nil {
		return nil, fmt.Errorf("unmarshal task result failed: %w", err)
	}

	taskResult := relaycommon.TaskInfo{
		Code: 0,
	}

	switch pResp.Status {
	case "pending":
		taskResult.Status = model.TaskStatusQueued
	case "done":
		taskResult.Status = model.TaskStatusSuccess
		if pResp.Video != nil {
			taskResult.Url = pResp.Video.URL
		}
	case "expired":
		taskResult.Status = model.TaskStatusFailure
		taskResult.Reason = "request expired"
	default:
		// xAI may omit the "status" field when video is ready,
		// returning only "video" + "model" in the response.
		if pResp.Video != nil && pResp.Video.URL != "" {
			taskResult.Status = model.TaskStatusSuccess
			taskResult.Url = pResp.Video.URL
		} else {
			taskResult.Status = model.TaskStatusQueued
		}
	}

	return &taskResult, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(task *model.Task) ([]byte, error) {
	ov := dto.NewOpenAIVideo()
	ov.ID = task.TaskID
	ov.Status = task.Status.ToVideoStatus()
	ov.SetProgressStr(task.Progress)
	ov.CreatedAt = task.CreatedAt
	if task.FinishTime > 0 {
		ov.CompletedAt = task.FinishTime
	} else if task.UpdatedAt > 0 {
		ov.CompletedAt = task.UpdatedAt
	}
	if task.Properties.OriginModelName != "" {
		ov.Model = task.Properties.OriginModelName
	}
	if task.FailReason != "" {
		if task.Status == model.TaskStatusSuccess {
			ov.SetMetadata("url", task.FailReason)
		} else if task.Status == model.TaskStatusFailure {
			ov.Error = &dto.OpenAIVideoError{
				Message: task.FailReason,
				Code:    "generation_failed",
			}
		}
	}

	return common.Marshal(ov)
}
