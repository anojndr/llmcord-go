package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultAutocompressorBaseURL      = "https://autocompressor.net"
	autocompressorTargetSize          = "8"
	autocompressorDefaultPollInterval = time.Second
	autocompressorMaxPollAttempts     = 120
	autocompressorUserAgent           = "Mozilla/5.0"
	autocompressorDefaultMIMEType     = "video/mp4"
)

const autocompressorRequestContextErrorFormat = "autocompressor request context: %w"

type autocompressorRQJobRequest struct {
	SourceType       string         `json:"source_type"`
	CompressionLevel string         `json:"compression_level"`
	TargetSize       string         `json:"target_size"`
	OutputFormat     string         `json:"output_format"`
	MoreOptions      map[string]any `json:"moreoptions"`
}

type autocompressorRQJobResponse struct {
	Allowed     bool   `json:"allowed"`
	Server      string `json:"server"`
	Message     string `json:"message"`
	UploadLimit int64  `json:"upload_limit"`
}

type autocompressorUploadResponse struct {
	Error any `json:"error"`
}

type autocompressorStatusResponse struct {
	Error  any `json:"error"`
	Status struct {
		Thumbnail bool `json:"thumbnail"`
		Ended     bool `json:"ended"`
		Error     any  `json:"error"`
	} `json:"status"`
	Progress struct {
		Action     string  `json:"action"`
		Quantified bool    `json:"quantified"`
		Progress   float64 `json:"progress"`
	} `json:"progress"`
}

func compressVideoViaAutocompressor(
	ctx context.Context,
	httpClient *http.Client,
	compressorURL string,
	pollInterval time.Duration,
	videoBytes []byte,
	filename string,
	defaultFilename string,
) ([]byte, string, string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(compressorURL), "/")
	if baseURL == "" {
		baseURL = defaultAutocompressorBaseURL
	}

	rqReqBody, err := json.Marshal(autocompressorRQJobRequest{
		SourceType:       "file",
		CompressionLevel: "normal",
		TargetSize:       autocompressorTargetSize,
		OutputFormat:     "mp4",
		MoreOptions: map[string]any{
			"av1webm": false,
			"dlaudio": false,
		},
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("marshal autocompressor rqjob request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+"/rqjob",
		bytes.NewReader(rqReqBody),
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("create autocompressor rqjob request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", autocompressorUserAgent)

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, "", "", fmt.Errorf("send autocompressor rqjob request: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read autocompressor rqjob response: %w", err)
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"autocompressor rqjob failed with status %d: %s: %w",
			httpResp.StatusCode,
			strings.TrimSpace(string(respBytes)),
			os.ErrInvalid,
		)
	}

	var rqResp autocompressorRQJobResponse

	err = json.Unmarshal(respBytes, &rqResp)
	if err != nil {
		return nil, "", "", fmt.Errorf("decode autocompressor rqjob response: %w", err)
	}

	if !rqResp.Allowed || strings.TrimSpace(rqResp.Message) == "" || strings.TrimSpace(rqResp.Server) == "" {
		return nil, "", "", fmt.Errorf("autocompressor rqjob not allowed: %s: %w", rqResp.Message, os.ErrInvalid)
	}

	jobID := strings.TrimSpace(rqResp.Message)
	server := strings.TrimSpace(rqResp.Server)

	var serverURL string
	if strings.Contains(baseURL, "://autocompressor.net") {
		serverURL = fmt.Sprintf("https://auto-rez-%s.autocompressor.net", server)
	} else {
		serverURL = baseURL
	}

	var bodyBuf bytes.Buffer

	mpWriter := multipart.NewWriter(&bodyBuf)

	_ = mpWriter.WriteField("source_url", "null")

	uploadFilename := strings.TrimSpace(filename)
	if uploadFilename == "" {
		uploadFilename = defaultFilename
	}

	part, err := mpWriter.CreateFormFile("filetoupload", uploadFilename)
	if err != nil {
		return nil, "", "", fmt.Errorf("create autocompressor form file: %w", err)
	}

	_, err = part.Write(videoBytes)
	if err != nil {
		return nil, "", "", fmt.Errorf("write autocompressor form file bytes: %w", err)
	}

	err = mpWriter.Close()
	if err != nil {
		return nil, "", "", fmt.Errorf("close autocompressor multipart writer: %w", err)
	}

	uploadReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/job/%s/upload", serverURL, jobID),
		&bodyBuf,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("create autocompressor upload request: %w", err)
	}

	uploadReq.Header.Set("Content-Type", mpWriter.FormDataContentType())
	uploadReq.Header.Set("User-Agent", autocompressorUserAgent)

	uploadResp, err := httpClient.Do(uploadReq)
	if err != nil {
		return nil, "", "", fmt.Errorf("send autocompressor upload request: %w", err)
	}
	defer func() {
		_ = uploadResp.Body.Close()
	}()

	uploadRespBytes, err := io.ReadAll(uploadResp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read autocompressor upload response: %w", err)
	}

	if uploadResp.StatusCode < http.StatusOK || uploadResp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"autocompressor upload failed with status %d: %s: %w",
			uploadResp.StatusCode,
			strings.TrimSpace(string(uploadRespBytes)),
			os.ErrInvalid,
		)
	}

	var uploadStatus autocompressorUploadResponse
	if err := json.Unmarshal(uploadRespBytes, &uploadStatus); err == nil {
		if uploadStatus.Error != nil && uploadStatus.Error != false {
			return nil, "", "", fmt.Errorf("autocompressor upload returned error: %v: %w", uploadStatus.Error, os.ErrInvalid)
		}
	}

	downloadURL := fmt.Sprintf("%s/job/%s/download", serverURL, jobID)
	statusURL := fmt.Sprintf("%s/job/%s/status", serverURL, jobID)

	for range autocompressorMaxPollAttempts {
		err := ctx.Err()
		if err != nil {
			return nil, "", "", fmt.Errorf(autocompressorRequestContextErrorFormat, err)
		}

		effectivePollInterval := pollInterval
		if effectivePollInterval <= 0 {
			effectivePollInterval = autocompressorDefaultPollInterval
		}

		select {
		case <-ctx.Done():
			return nil, "", "", fmt.Errorf(autocompressorRequestContextErrorFormat, ctx.Err())
		case <-time.After(effectivePollInterval):
		}

		statusReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			statusURL,
			nil,
		)
		if err != nil {
			return nil, "", "", fmt.Errorf("create autocompressor status request: %w", err)
		}

		statusReq.Header.Set("User-Agent", autocompressorUserAgent)

		statusResp, err := httpClient.Do(statusReq)
		if err != nil {
			return nil, "", "", fmt.Errorf("send autocompressor status request: %w", err)
		}

		statusBytes, err := io.ReadAll(statusResp.Body)
		_ = statusResp.Body.Close()

		if err != nil {
			return nil, "", "", fmt.Errorf("read autocompressor status response: %w", err)
		}

		if statusResp.StatusCode < http.StatusOK || statusResp.StatusCode >= http.StatusMultipleChoices {
			return nil, "", "", fmt.Errorf(
				"autocompressor status failed with status %d: %s: %w",
				statusResp.StatusCode,
				strings.TrimSpace(string(statusBytes)),
				os.ErrInvalid,
			)
		}

		var statusData autocompressorStatusResponse

		err = json.Unmarshal(statusBytes, &statusData)
		if err != nil {
			return nil, "", "", fmt.Errorf("decode autocompressor status response: %w", err)
		}

		if statusData.Status.Ended {
			if statusData.Status.Error != nil && statusData.Status.Error != false {
				return nil, "", "", fmt.Errorf("autocompressor job failed: %v: %w", statusData.Status.Error, os.ErrInvalid)
			}

			return downloadAutocompressedVideo(ctx, httpClient, downloadURL, filename, defaultFilename)
		}
	}

	return nil, "", "", fmt.Errorf("autocompressor job timed out: %w", os.ErrDeadlineExceeded)
}

func downloadAutocompressedVideo(
	ctx context.Context,
	httpClient *http.Client,
	downloadURL string,
	originalFilename string,
	defaultFilename string,
) ([]byte, string, string, error) {
	dlReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		downloadURL,
		nil,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("create autocompressor download request: %w", err)
	}

	dlReq.Header.Set("User-Agent", autocompressorUserAgent)

	dlResp, err := httpClient.Do(dlReq)
	if err != nil {
		return nil, "", "", fmt.Errorf("send autocompressor download request: %w", err)
	}
	defer func() {
		_ = dlResp.Body.Close()
	}()

	dlBytes, err := io.ReadAll(dlResp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read autocompressor download response: %w", err)
	}

	if dlResp.StatusCode < http.StatusOK || dlResp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", "", fmt.Errorf(
			"autocompressor download failed with status %d: %s: %w",
			dlResp.StatusCode,
			strings.TrimSpace(string(dlBytes)),
			os.ErrInvalid,
		)
	}

	if len(dlBytes) == 0 {
		return nil, "", "", fmt.Errorf("empty autocompressor download response: %w", os.ErrInvalid)
	}

	mimeType := normalizeAutocompressedMIMEType(dlResp.Header.Get("Content-Type"))
	filename := originalFilename

	contentDisposition := dlResp.Header.Get("Content-Disposition")
	if strings.TrimSpace(contentDisposition) != "" {
		_, params, err := mime.ParseMediaType(contentDisposition)
		if err == nil {
			dispositionFilename := strings.TrimSpace(params["filename"])
			if dispositionFilename != "" {
				filename = dispositionFilename
			}
		}
	}

	if strings.TrimSpace(filename) == "" {
		filename = defaultFilename
	}

	return dlBytes, mimeType, filename, nil
}

func normalizeAutocompressedMIMEType(contentType string) string {
	trimmedContentType := strings.TrimSpace(contentType)
	if trimmedContentType == "" {
		return autocompressorDefaultMIMEType
	}

	mediaType, _, err := mime.ParseMediaType(trimmedContentType)
	if err != nil {
		return autocompressorDefaultMIMEType
	}

	if strings.TrimSpace(mediaType) == "" {
		return autocompressorDefaultMIMEType
	}

	if strings.EqualFold(mediaType, "application/octet-stream") {
		return autocompressorDefaultMIMEType
	}

	return mediaType
}
