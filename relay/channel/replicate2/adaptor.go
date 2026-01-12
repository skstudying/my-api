package replicate2

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type Adaptor struct{}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info == nil {
		return "", errors.New("replicate2 adaptor: relay info is nil")
	}
	if info.ChannelBaseUrl == "" {
		info.ChannelBaseUrl = constant.ChannelBaseURLs[constant.ChannelTypeReplicate2]
	}
	// img2img 使用 /v1/predictions 端点
	return relaycommon.GetFullRequestURL(info.ChannelBaseUrl, "/v1/predictions", info.ChannelType), nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	if info == nil {
		return errors.New("replicate2 adaptor: relay info is nil")
	}
	if info.ApiKey == "" {
		return errors.New("replicate2 adaptor: api key is required")
	}
	channel.SetupApiRequestHeader(info, c, req)
	req.Set("Authorization", "Bearer "+info.ApiKey)
	req.Set("Prefer", "wait")
	if req.Get("Content-Type") == "" {
		req.Set("Content-Type", "application/json")
	}
	if req.Get("Accept") == "" {
		req.Set("Accept", "application/json")
	}
	return nil
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	if info == nil {
		return nil, errors.New("replicate2 adaptor: relay info is nil")
	}

	// 获取模型名和版本
	modelName := strings.TrimSpace(info.UpstreamModelName)
	if modelName == "" {
		modelName = strings.TrimSpace(request.Model)
	}

	// 从模型名中提取版本 ID
	// 格式可能是: owner/model:version 或 owner/model-img2img:version
	version := extractVersionFromModel(modelName)

	// 如果模型名中没有版本，尝试从 Extra 字段获取
	if version == "" {
		if v, ok := getExtraField(request.Extra, "version"); ok {
			version = v
		}
	}

	if version == "" {
		return nil, errors.New("replicate2 adaptor: version is required for img2img, please specify in model name (owner/model:version) or in 'version' field")
	}

	// 构建 input payload
	inputPayload := make(map[string]any)

	// 处理 prompt
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		if v := c.PostForm("prompt"); strings.TrimSpace(v) != "" {
			prompt = v
		}
	}
	if prompt != "" {
		inputPayload["prompt"] = prompt
	}

	// 处理图片 - 从多个来源获取
	imageURL, err := getImageFromRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	if imageURL == "" {
		return nil, errors.New("replicate2 adaptor: image is required for img2img")
	}
	inputPayload["image"] = imageURL

	// 处理 strength (图生图强度)
	if strength, ok := getExtraFieldFloat(request.Extra, "strength"); ok {
		inputPayload["strength"] = strength
	} else {
		// 默认 strength
		inputPayload["strength"] = 0.6
	}

	// 处理 output_format
	if len(request.OutputFormat) > 0 {
		var outputFormat string
		if err := common.Unmarshal(request.OutputFormat, &outputFormat); err == nil && strings.TrimSpace(outputFormat) != "" {
			inputPayload["output_format"] = outputFormat
		}
	}

	// 处理其他额外参数
	extraParams := []string{"guidance_scale", "output_quality", "num_inference_steps", "seed", "negative_prompt"}
	for _, param := range extraParams {
		if val, ok := getExtraFieldAny(request.Extra, param); ok {
			inputPayload[param] = val
		}
	}

	// 处理 ExtraFields (额外字段)
	if len(request.ExtraFields) > 0 {
		var extra map[string]any
		if err := common.Unmarshal(request.ExtraFields, &extra); err == nil {
			for key, val := range extra {
				if key != "version" { // version 已单独处理
					inputPayload[key] = val
				}
			}
		}
	}

	// 处理 Extra 中的 input 字段 (深层合并)
	for key, raw := range request.Extra {
		if strings.EqualFold(key, "input") {
			var extraInput map[string]any
			if err := common.Unmarshal(raw, &extraInput); err == nil {
				for k, v := range extraInput {
					inputPayload[k] = v
				}
			}
			continue
		}
		// 跳过已处理的字段
		if key == "version" || key == "strength" || contains(extraParams, key) {
			continue
		}
		if raw == nil {
			continue
		}
		var val any
		if err := common.Unmarshal(raw, &val); err == nil {
			inputPayload[key] = val
		}
	}

	return PredictionRequest{
		Version: version,
		Input:   inputPayload,
	}, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if resp == nil {
		return nil, types.NewError(errors.New("replicate2 adaptor: empty response"), types.ErrorCodeBadResponse)
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	_ = resp.Body.Close()

	var prediction PredictionResponse
	if err := common.Unmarshal(responseBody, &prediction); err != nil {
		return nil, types.NewError(fmt.Errorf("replicate2 adaptor: failed to decode response: %w", err), types.ErrorCodeBadResponseBody)
	}

	if prediction.Error != nil {
		errMsg := prediction.Error.Message
		if errMsg == "" {
			errMsg = prediction.Error.Detail
		}
		if errMsg == "" {
			errMsg = prediction.Error.Code
		}
		if errMsg == "" {
			errMsg = "replicate2 adaptor: prediction error"
		}
		return nil, types.NewError(errors.New(errMsg), types.ErrorCodeBadResponse)
	}

	if prediction.Status != "" && !strings.EqualFold(prediction.Status, "succeeded") {
		return nil, types.NewError(fmt.Errorf("replicate2 adaptor: prediction status %q", prediction.Status), types.ErrorCodeBadResponse)
	}

	var urls []string

	appendOutput := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		urls = append(urls, value)
	}

	switch output := prediction.Output.(type) {
	case string:
		appendOutput(output)
	case []any:
		for _, item := range output {
			if str, ok := item.(string); ok {
				appendOutput(str)
			}
		}
	case nil:
		// no output
	default:
		if str, ok := output.(fmt.Stringer); ok {
			appendOutput(str.String())
		}
	}

	if len(urls) == 0 {
		return nil, types.NewError(errors.New("replicate2 adaptor: empty prediction output"), types.ErrorCodeBadResponseBody)
	}

	var imageReq *dto.ImageRequest
	if info != nil {
		if req, ok := info.Request.(*dto.ImageRequest); ok {
			imageReq = req
		}
	}

	wantsBase64 := imageReq != nil && strings.EqualFold(imageReq.ResponseFormat, "b64_json")

	imageResponse := dto.ImageResponse{
		Created: common.GetTimestamp(),
		Data:    make([]dto.ImageData, 0),
	}

	if wantsBase64 {
		converted, convErr := downloadImagesToBase64(urls)
		if convErr != nil {
			return nil, types.NewError(convErr, types.ErrorCodeBadResponse)
		}
		for _, content := range converted {
			if content == "" {
				continue
			}
			imageResponse.Data = append(imageResponse.Data, dto.ImageData{B64Json: content})
		}
	} else {
		for _, url := range urls {
			if url == "" {
				continue
			}
			imageResponse.Data = append(imageResponse.Data, dto.ImageData{Url: url})
		}
	}

	if len(imageResponse.Data) == 0 {
		return nil, types.NewError(errors.New("replicate2 adaptor: no usable image data"), types.ErrorCodeBadResponse)
	}

	responseBytes, err := common.Marshal(imageResponse)
	if err != nil {
		return nil, types.NewError(fmt.Errorf("replicate2 adaptor: encode response failed: %w", err), types.ErrorCodeBadResponseBody)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(responseBytes)

	usage := &dto.Usage{}
	return usage, nil
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

// Helper functions

func extractVersionFromModel(model string) string {
	// 格式: owner/model:version_hash
	if idx := strings.LastIndex(model, ":"); idx != -1 {
		return model // 返回完整的 owner/model:version
	}
	return ""
}

func getExtraField(extra map[string]any, key string) (string, bool) {
	if extra == nil {
		return "", false
	}
	if val, ok := extra[key]; ok {
		if str, ok := val.(string); ok {
			return str, true
		}
	}
	return "", false
}

func getExtraFieldFloat(extra map[string]any, key string) (float64, bool) {
	if extra == nil {
		return 0, false
	}
	if val, ok := extra[key]; ok {
		switch v := val.(type) {
		case float64:
			return v, true
		case float32:
			return float64(v), true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		}
	}
	return 0, false
}

func getExtraFieldAny(extra map[string]any, key string) (any, bool) {
	if extra == nil {
		return nil, false
	}
	if val, ok := extra[key]; ok {
		return val, true
	}
	return nil, false
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func getImageFromRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (string, error) {
	// 1. 首先尝试从 request.Image 获取 (JSON 请求中的 image 字段)
	if len(request.Image) > 0 {
		var imageValue string
		if err := common.Unmarshal(request.Image, &imageValue); err == nil && imageValue != "" {
			// 检查是否是 base64 数据
			if strings.HasPrefix(imageValue, "data:") {
				// data:image/png;base64,... 格式，需要上传到 Replicate
				return uploadBase64Image(info, imageValue)
			}
			// 假设是 URL
			return imageValue, nil
		}
	}

	// 2. 尝试从 Extra 字段获取
	if imageURL, ok := getExtraField(request.Extra, "image"); ok && imageURL != "" {
		if strings.HasPrefix(imageURL, "data:") {
			return uploadBase64Image(info, imageURL)
		}
		return imageURL, nil
	}

	// 3. 尝试从 multipart form 获取
	if c.Request.MultipartForm != nil || strings.Contains(c.ContentType(), "multipart/form-data") {
		imageURL, err := uploadFileFromForm(c, info)
		if err != nil {
			return "", err
		}
		if imageURL != "" {
			return imageURL, nil
		}
	}

	return "", nil
}

func uploadBase64Image(info *relaycommon.RelayInfo, dataURL string) (string, error) {
	// 解析 data URL: data:image/png;base64,xxxx
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return "", errors.New("replicate2 adaptor: invalid data URL format")
	}

	// 解码 base64
	decoded, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("replicate2 adaptor: failed to decode base64 image: %w", err)
	}

	// 确定 content type
	contentType := "application/octet-stream"
	if strings.Contains(parts[0], "image/png") {
		contentType = "image/png"
	} else if strings.Contains(parts[0], "image/jpeg") || strings.Contains(parts[0], "image/jpg") {
		contentType = "image/jpeg"
	} else if strings.Contains(parts[0], "image/webp") {
		contentType = "image/webp"
	}

	// 上传到 Replicate
	return uploadToReplicate(info, decoded, "image.png", contentType)
}

func uploadFileFromForm(c *gin.Context, info *relaycommon.RelayInfo) (string, error) {
	mf := c.Request.MultipartForm
	if mf == nil {
		if _, err := c.MultipartForm(); err != nil {
			return "", nil // 不是 multipart 请求，返回空
		}
		mf = c.Request.MultipartForm
	}
	if mf == nil || len(mf.File) == 0 {
		return "", nil
	}

	fieldCandidates := []string{"image", "image[]", "file"}
	var fileHeader *multipart.FileHeader

	for _, key := range fieldCandidates {
		if files := mf.File[key]; len(files) > 0 {
			fileHeader = files[0]
			break
		}
	}
	if fileHeader == nil {
		for _, files := range mf.File {
			if len(files) > 0 {
				fileHeader = files[0]
				break
			}
		}
	}
	if fileHeader == nil {
		return "", nil
	}

	file, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("replicate2 adaptor: failed to open image file: %w", err)
	}
	defer file.Close()

	fileContent, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("replicate2 adaptor: failed to read image file: %w", err)
	}

	contentType := fileHeader.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return uploadToReplicate(info, fileContent, fileHeader.Filename, contentType)
}

func uploadToReplicate(info *relaycommon.RelayInfo, fileContent []byte, filename, contentType string) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf("form-data; name=\"content\"; filename=\"%s\"", filename))
	hdr.Set("Content-Type", contentType)

	part, err := writer.CreatePart(hdr)
	if err != nil {
		writer.Close()
		return "", fmt.Errorf("replicate2 adaptor: create upload form failed: %w", err)
	}
	if _, err := part.Write(fileContent); err != nil {
		writer.Close()
		return "", fmt.Errorf("replicate2 adaptor: write image content failed: %w", err)
	}
	formContentType := writer.FormDataContentType()
	writer.Close()

	baseURL := info.ChannelBaseUrl
	if baseURL == "" {
		baseURL = constant.ChannelBaseURLs[constant.ChannelTypeReplicate2]
	}
	uploadURL := relaycommon.GetFullRequestURL(baseURL, "/v1/files", info.ChannelType)

	req, err := http.NewRequest(http.MethodPost, uploadURL, &body)
	if err != nil {
		return "", fmt.Errorf("replicate2 adaptor: create upload request failed: %w", err)
	}
	req.Header.Set("Content-Type", formContentType)
	req.Header.Set("Authorization", "Bearer "+info.ApiKey)

	resp, err := service.GetHttpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("replicate2 adaptor: upload image failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("replicate2 adaptor: read upload response failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("replicate2 adaptor: upload image failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var uploadResp struct {
		Urls struct {
			Get string `json:"get"`
		} `json:"urls"`
	}
	if err := common.Unmarshal(respBody, &uploadResp); err != nil {
		return "", fmt.Errorf("replicate2 adaptor: decode upload response failed: %w", err)
	}
	if uploadResp.Urls.Get == "" {
		return "", errors.New("replicate2 adaptor: upload response missing url")
	}
	return uploadResp.Urls.Get, nil
}

func downloadImagesToBase64(urls []string) ([]string, error) {
	results := make([]string, 0, len(urls))
	for _, url := range urls {
		if strings.TrimSpace(url) == "" {
			continue
		}
		_, data, err := service.GetImageFromUrl(url)
		if err != nil {
			return nil, fmt.Errorf("replicate2 adaptor: failed to download image from %s: %w", url, err)
		}
		results = append(results, data)
	}
	return results, nil
}

// Unimplemented methods for Adaptor interface

func (a *Adaptor) ConvertOpenAIRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeneralOpenAIRequest) (any, error) {
	return nil, errors.New("replicate2 adaptor: ConvertOpenAIRequest is not implemented")
}

func (a *Adaptor) ConvertRerankRequest(*gin.Context, int, dto.RerankRequest) (any, error) {
	return nil, errors.New("replicate2 adaptor: ConvertRerankRequest is not implemented")
}

func (a *Adaptor) ConvertEmbeddingRequest(*gin.Context, *relaycommon.RelayInfo, dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("replicate2 adaptor: ConvertEmbeddingRequest is not implemented")
}

func (a *Adaptor) ConvertAudioRequest(*gin.Context, *relaycommon.RelayInfo, dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("replicate2 adaptor: ConvertAudioRequest is not implemented")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(*gin.Context, *relaycommon.RelayInfo, dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("replicate2 adaptor: ConvertOpenAIResponsesRequest is not implemented")
}

func (a *Adaptor) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("replicate2 adaptor: ConvertClaudeRequest is not implemented")
}

func (a *Adaptor) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("replicate2 adaptor: ConvertGeminiRequest is not implemented")
}

