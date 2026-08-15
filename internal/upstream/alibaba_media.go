package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Alibaba Media Studio — DashScope image generation / editing / video.
// Endpoints verified live 2026-08-15:
//   - Image gen/edit: POST /api/v1/services/aigc/multimodal-generation/generation (sync)
//   - Video gen:      POST /api/v1/services/aigc/video-generation/video-synthesis (async, X-DashScope-Async)
//   - Task poll:      GET  /api/v1/tasks/{task_id}
//   - File upload:    POST /api/v1/files (multipart) -> GET /api/v1/files/{id} -> signed URL
// No session lifecycle (plain sk-ws-* Bearer keys) — no SSO, no DPoP, no Turnstile.

const (
	aliMediaBase        = "https://dashscope-intl.aliyuncs.com"
	aliMediaTimeout     = 240 * time.Second // qwen-image-3.0 gen can take ~60s+
	aliMediaMaxBody     = 64 << 20
	aliImageGenDefault  = "qwen-image-3.0"
	aliImageEditDefault = "qwen-image-edit"
	aliVideoDefault     = "wan2.6-t2v"
)

// AliMediaModels — gateway media model ids (also accepted with ali/ prefix).
var AliMediaModels = []string{
	"qwen-image-3.0", "qwen-image-3.0-pro", "qwen-image-2.0-pro", "qwen-image-2.0",
	"qwen-image-max", "qwen-image-plus", "z-image-turbo",
	"wan2.7-image-pro", "wan2.7-image", "wan2.6-image", "wan2.6-t2i", "wan2.2-t2i-plus",
	"qwen-image-edit", "qwen-image-edit-plus", "qwen-image-edit-max",
	"wan2.7-t2v", "wan2.6-t2v", "wan2.2-t2v-plus", "wan2.1-t2v-turbo", "wan3.0-video",
	"wan2.7-i2v", "wan2.6-i2v", "wan2.1-i2v-turbo",
}

// aliIsMediaModel reports whether a model id is one of the media models
// (strips a leading "ali/" first). Unknown models fall back to the default.
func aliIsMediaModel(model string) bool {
	model = strings.TrimPrefix(strings.TrimSpace(model), "ali/")
	for _, m := range AliMediaModels {
		if m == model {
			return true
		}
	}
	return false
}

// aliMediaSize normalizes "1024x1024" -> "1024*1024" (DashScope format).
func aliMediaSize(size string) string {
	s := strings.TrimSpace(size)
	if s == "" {
		return "1024*1024"
	}
	return strings.ReplaceAll(s, "x", "*")
}

// AliImageGen generates images synchronously. Returns the public image URLs.
func (am *AlibabaKeyManager) AliImageGen(model, prompt, size string, n int) ([]string, error) {
	key, err := am.Next()
	if err != nil {
		return nil, err
	}
	if !aliIsMediaModel(model) {
		model = aliImageGenDefault
	}
	if n <= 0 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	body := map[string]any{
		"model": model,
		"input": map[string]any{
			"messages": []any{
				map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"text": prompt}},
				},
			},
		},
		"parameters": map[string]any{
			"size":      aliMediaSize(size),
			"watermark": false,
			"n":         n,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := aliMediaDo(key.Key, "POST", "/api/v1/services/aigc/multimodal-generation/generation", raw)
	if err != nil {
		return nil, err
	}
	urls := aliExtractImageURLs(resp)
	if len(urls) == 0 {
		return nil, fmt.Errorf("dashscope returned no images: %s", aliErrMsg(resp))
	}
	return urls, nil
}

// AliUploadImage uploads a base64 image to the DashScope Files API and
// returns a public signed URL. DashScope image editing requires a public
// URL (base64/data-URI/file_id are rejected).
func (am *AlibabaKeyManager) AliUploadImage(imageB64, filename string) (string, error) {
	key, err := am.Next()
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(imageB64)
	if err != nil {
		return "", fmt.Errorf("invalid base64 image")
	}
	if filename == "" {
		filename = "edit.png"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("upload_file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	w.Close()

	req, err := http.NewRequest("POST", aliMediaBase+"/api/v1/files", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := &http.Client{Timeout: aliMediaTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dashscope file upload failed: %d", resp.StatusCode)
	}
	var up struct {
		Data struct {
			UploadedFiles []struct {
				FileID string `json:"file_id"`
			} `json:"uploaded_files"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, aliMediaMaxBody)).Decode(&up); err != nil {
		return "", err
	}
	if len(up.Data.UploadedFiles) == 0 {
		return "", fmt.Errorf("dashscope file upload returned no file_id")
	}
	fid := up.Data.UploadedFiles[0].FileID

	// Resolve file_id -> public signed URL.
	greq, err := http.NewRequest("GET", aliMediaBase+"/api/v1/files/"+fid, nil)
	if err != nil {
		return "", err
	}
	greq.Header.Set("Authorization", "Bearer "+key.Key)
	gresp, err := client.Do(greq)
	if err != nil {
		return "", err
	}
	defer gresp.Body.Close()
	var gd struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(gresp.Body, aliMediaMaxBody)).Decode(&gd); err != nil {
		return "", err
	}
	if gd.Data.URL == "" {
		return "", fmt.Errorf("dashscope file url empty")
	}
	return gd.Data.URL, nil
}

// AliImageEdit edits an image. imageRef may be a base64 blob or a public URL.
func (am *AlibabaKeyManager) AliImageEdit(model, prompt, imageRef string) (string, error) {
	key, err := am.Next()
	if err != nil {
		return "", err
	}
	if !aliIsMediaModel(model) {
		model = aliImageEditDefault
	}
	img := imageRef
	if strings.HasPrefix(imageRef, "data:") {
		// data:image/...;base64,XXXX
		if i := strings.Index(imageRef, ","); i > 0 {
			img = imageRef[i+1:]
		}
	}
	if img != "" && !strings.HasPrefix(img, "http") {
		url, err := am.AliUploadImage(img, "edit.png")
		if err != nil {
			return "", err
		}
		img = url
	}
	if img == "" {
		return "", fmt.Errorf("image is required for edit")
	}
	body := map[string]any{
		"model": model,
		"input": map[string]any{
			"messages": []any{
				map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"text": prompt}, map[string]any{"image": img}},
				},
			},
		},
		"parameters": map[string]any{"size": "1024*1024", "watermark": false},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	resp, err := aliMediaDo(key.Key, "POST", "/api/v1/services/aigc/multimodal-generation/generation", raw)
	if err != nil {
		return "", err
	}
	urls := aliExtractImageURLs(resp)
	if len(urls) == 0 {
		return "", fmt.Errorf("dashscope edit returned no image: %s", aliErrMsg(resp))
	}
	return urls[0], nil
}

// AliVideoCreate starts an async video generation task and returns its id.
func (am *AlibabaKeyManager) AliVideoCreate(model, prompt string) (string, error) {
	key, err := am.Next()
	if err != nil {
		return "", err
	}
	if !aliIsMediaModel(model) {
		model = aliVideoDefault
	}
	body := map[string]any{
		"model": model,
		"input": map[string]any{"prompt": prompt},
		"parameters": map[string]any{
			"size": "1280*720",
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	resp, err := aliMediaDo(key.Key, "POST", "/api/v1/services/aigc/video-generation/video-synthesis", raw)
	if err != nil {
		return "", err
	}
	var o struct {
		Output struct {
			TaskID string `json:"task_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(resp, &o); err != nil {
		return "", err
	}
	if o.Output.TaskID == "" {
		return "", fmt.Errorf("dashscope video create returned no task_id: %s", aliErrMsg(resp))
	}
	return o.Output.TaskID, nil
}

// AliVideoStatus polls an async task. Returns (status, videoURL, error).
// status is one of "pending", "running", "succeeded", "failed", "unknown".
func (am *AlibabaKeyManager) AliVideoStatus(taskID string) (string, string, error) {
	key, err := am.Next()
	if err != nil {
		return "", "", err
	}
	resp, err := aliMediaDo(key.Key, "GET", "/api/v1/tasks/"+taskID, nil)
	if err != nil {
		return "", "", err
	}
	var o struct {
		Output struct {
			TaskStatus string `json:"task_status"`
			VideoURL   string `json:"video_url"`
		} `json:"output"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(resp, &o); err != nil {
		return "", "", err
	}
	status := strings.ToLower(o.Output.TaskStatus)
	if status == "" {
		status = "unknown"
	}
	return status, o.Output.VideoURL, nil
}

// ---- internal helpers ------------------------------------------------------

// aliMediaDo performs an authenticated DashScope media call and returns the
// raw JSON body. Header X-DashScope-Async is set for async-capable paths.
func aliMediaDo(key, method, path string, raw []byte) ([]byte, error) {
	var reader io.Reader
	if raw != nil {
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, aliMediaBase+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.Contains(path, "video-generation") {
		req.Header.Set("X-DashScope-Async", "enable")
	}
	client := &http.Client{Timeout: aliMediaTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, aliMediaMaxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dashscope media %s %s: HTTP %d: %s", method, path, resp.StatusCode, aliErrMsg(body))
	}
	return body, nil
}

// aliExtractImageURLs pulls image urls from a multimodal-generation response.
func aliExtractImageURLs(body []byte) []string {
	var d struct {
		Output struct {
			Choices []struct {
				Message struct {
					Content []struct {
						Image string `json:"image"`
					} `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil
	}
	var urls []string
	for _, ch := range d.Output.Choices {
		for _, c := range ch.Message.Content {
			if c.Image != "" {
				urls = append(urls, c.Image)
			}
		}
	}
	return urls
}

// aliErrMsg extracts a human message from a DashScope error body.
func aliErrMsg(body []byte) string {
	var d struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return strings.TrimSpace(string(body))[:min(len(body), 200)]
	}
	if d.Message != "" {
		return d.Code + ": " + d.Message
	}
	return strings.TrimSpace(string(body))[:min(len(body), 200)]
}
