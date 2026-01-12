package replicate2

// PredictionRequest represents the request body for Replicate img2img predictions
type PredictionRequest struct {
	Version string         `json:"version"`
	Input   map[string]any `json:"input"`
}

// PredictionResponse represents the response from Replicate predictions API
type PredictionResponse struct {
	ID     string           `json:"id"`
	Status string           `json:"status"`
	Output any              `json:"output"`
	Error  *PredictionError `json:"error"`
}

// PredictionError represents an error in the prediction response
type PredictionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

