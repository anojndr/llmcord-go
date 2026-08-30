package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type stubImageSearchClient struct {
	searchFn   func(ctx context.Context, query string, page int, pageSize int) (*imageSearchResult, error)
	downloadFn func(ctx context.Context, items []imageSearchResultItem) []imageSearchResultItem
}

func (s *stubImageSearchClient) search(ctx context.Context, query string, page int, pageSize int) (*imageSearchResult, error) {
	if s.searchFn != nil {
		return s.searchFn(ctx, query, page, pageSize)
	}
	return &imageSearchResult{
		Query:      query,
		Page:       page,
		TotalPages: 1,
		Items: []imageSearchResultItem{
			{
				Title:      "Sample Image 1",
				URL:        "https://example.com/image1.jpg",
				Thumbnail:  "https://example.com/thumb1.jpg",
				LandingURL: "https://example.com/view1",
				Source:     "Test",
			},
			{
				Title:      "Sample Image 2",
				URL:        "https://example.com/image2.jpg",
				Thumbnail:  "https://example.com/thumb2.jpg",
				LandingURL: "https://example.com/view2",
				Source:     "Test",
			},
		},
	}, nil
}

func (s *stubImageSearchClient) downloadImages(ctx context.Context, items []imageSearchResultItem) []imageSearchResultItem {
	if s.downloadFn != nil {
		return s.downloadFn(ctx, items)
	}
	results := make([]imageSearchResultItem, len(items))
	copy(results, items)
	for i := range results {
		results[i].Data = []byte("image-data-" + strconvItoa(i+1))
		results[i].Filename = "image_" + strconvItoa(i+1) + ".jpg"
		results[i].ContentType = "image/jpeg"
	}
	return results
}

func strconvItoa(i int) string {
	if i == 1 {
		return "1"
	}
	if i == 2 {
		return "2"
	}
	if i == 3 {
		return "3"
	}
	if i == 4 {
		return "4"
	}
	return "5"
}

func newShowImagesInteraction() *discordgo.InteractionCreate {
	return newComponentInteraction("response-message", showImagesButtonCustomID)
}

func newShowImagesPageInteraction(messageID string, pageIndex int) *discordgo.InteractionCreate {
	return newComponentInteraction(messageID, showImagesPageButtonCustomID(messageID, pageIndex))
}

func TestHandleInteractionCreateRespondsToShowImagesButtonSendsImages(t *testing.T) {
	t.Parallel()

	var (
		initialResponse discordgo.InteractionResponse
		webhookEdited   bool
		editedContent   string
		editedEmbeds    []*discordgo.MessageEmbed
		editedFiles     []*discordgo.File
	)

	session := newInteractionTestSessionWithTransport(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if strings.HasSuffix(request.URL.Path, "/callback") {
			return captureInteractionCallbackRequest(t, request, &initialResponse)
		}

		if strings.Contains(request.URL.Path, "/webhooks/") && strings.Contains(request.URL.Path, "/messages/@original") {
			webhookEdited = true
			contentType := request.Header.Get("Content-Type")
			if strings.HasPrefix(contentType, "multipart/form-data") {
				// Parse multipart
				err := request.ParseMultipartForm(10 * 1024 * 1024)
				if err == nil {
					payloadJSON := request.FormValue("payload_json")
					if payloadJSON != "" {
						var parsed discordgo.WebhookEdit
						if jsonErr := json.Unmarshal([]byte(payloadJSON), &parsed); jsonErr == nil {
							if parsed.Content != nil {
								editedContent = *parsed.Content
							}
							if parsed.Embeds != nil {
								editedEmbeds = *parsed.Embeds
							}
						}
					}
					if request.MultipartForm != nil && request.MultipartForm.File != nil {
						for _, headers := range request.MultipartForm.File {
							for _, fh := range headers {
								f, openErr := fh.Open()
								if openErr == nil {
									data, _ := io.ReadAll(f)
									_ = f.Close()
									editedFiles = append(editedFiles, &discordgo.File{
										Name:   fh.Filename,
										Reader: bytes.NewReader(data),
									})
								}
							}
						}
					}
				}
			} else {
				body, _ := io.ReadAll(request.Body)
				var parsed discordgo.WebhookEdit
				if jsonErr := json.Unmarshal(body, &parsed); jsonErr == nil {
					if parsed.Content != nil {
						editedContent = *parsed.Content
					}
					if parsed.Embeds != nil {
						editedEmbeds = *parsed.Embeds
					}
				}
			}

			return newJSONResponse(t, request, &discordgo.Message{ID: "response-message"}), nil
		}

		return newNoContentResponse(request), nil
	}))

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.imageSearch = &stubImageSearchClient{}

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.searchMetadata = &searchMetadata{
		Queries: []string{"golden retriever puppy"},
		Results: []webSearchResult{{
			Query: "golden retriever puppy",
			Text:  "Title: Example Source\nURL: https://example.com/source\n",
		}},
		MaxURLs: defaultWebSearchMaxURLs,
	}
	node.mu.Unlock()

	interaction := newShowImagesInteraction()
	instance.handleInteractionCreate(session, interaction)

	if initialResponse.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Fatalf("expected deferred interaction response type %v, got %v", discordgo.InteractionResponseDeferredChannelMessageWithSource, initialResponse.Type)
	}

	if initialResponse.Data != nil && initialResponse.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("unexpected flags: %v", initialResponse.Data.Flags)
	}

	if !webhookEdited {
		t.Fatal("expected webhook edit for interaction response")
	}

	if !containsFold(editedContent, "Top 2 images for \"golden retriever puppy\"") {
		t.Fatalf("unexpected edited content: %q", editedContent)
	}

	if len(editedEmbeds) != 2 {
		t.Fatalf("expected 2 embeds, got %d", len(editedEmbeds))
	}

	if len(editedFiles) != 2 {
		t.Fatalf("expected 2 attached image files, got %d", len(editedFiles))
	}

	if editedEmbeds[0].Image == nil || !strings.HasPrefix(editedEmbeds[0].Image.URL, "attachment://") {
		t.Fatalf("expected image attachment URL in embed, got: %#v", editedEmbeds[0].Image)
	}
}

func TestHandleInteractionCreateRespondsToShowImagesButtonFromParentMessage(t *testing.T) {
	t.Parallel()

	var (
		initialResponse discordgo.InteractionResponse
		webhookEdited   bool
		editedContent   string
	)

	session := newInteractionTestSessionWithTransport(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Helper()

		if strings.HasSuffix(request.URL.Path, "/callback") {
			return captureInteractionCallbackRequest(t, request, &initialResponse)
		}

		if strings.Contains(request.URL.Path, "/webhooks/") && strings.Contains(request.URL.Path, "/messages/@original") {
			webhookEdited = true
			contentType := request.Header.Get("Content-Type")
			if strings.HasPrefix(contentType, "multipart/form-data") {
				_ = request.ParseMultipartForm(10 * 1024 * 1024)
				payloadJSON := request.FormValue("payload_json")
				if payloadJSON != "" {
					var parsed discordgo.WebhookEdit
					_ = json.Unmarshal([]byte(payloadJSON), &parsed)
					if parsed.Content != nil {
						editedContent = *parsed.Content
					}
				}
			}

			return newJSONResponse(t, request, &discordgo.Message{ID: "response-message"}), nil
		}

		return newNoContentResponse(request), nil
	}))

	instance := new(bot)
	instance.nodes = newMessageNodeStore(10)
	instance.imageSearch = &stubImageSearchClient{}

	parentMessage := &discordgo.Message{
		ID:      "parent-message",
		Content: "<@123456> show me pictures of cats",
	}

	node := instance.nodes.getOrCreate("response-message")
	node.mu.Lock()
	node.parentMessage = parentMessage
	node.mu.Unlock()

	interaction := newShowImagesInteraction()
	instance.handleInteractionCreate(session, interaction)

	if !webhookEdited {
		t.Fatal("expected webhook edit")
	}

	if !containsFold(editedContent, "show me pictures of cats") {
		t.Fatalf("expected parent message content query: %q", editedContent)
	}
}

func TestImageSearchClientOpenverseAndWikimedia(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/v1/images") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"result_count": 2,
				"page_count": 2,
				"results": [
					{
						"title": "Openverse Image 1",
						"url": "https://example.com/ov1.jpg",
						"thumbnail": "https://example.com/ov1_thumb.jpg",
						"foreign_landing_url": "https://example.com/landing1",
						"creator": "Author 1",
						"source": "flickr"
					}
				]
			}`))
			return
		}

		if strings.Contains(request.URL.Path, "api.php") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"query": {
					"pages": {
						"123": {
							"title": "File:Wiki Image.jpg",
							"imageinfo": [
								{
									"url": "https://example.com/wiki.jpg",
									"thumburl": "https://example.com/wiki_thumb.jpg",
									"descriptionurl": "https://commons.wikimedia.org/wiki/File:Wiki_Image.jpg",
									"mime": "image/jpeg"
								}
							]
						}
					}
				}
			}`))
			return
		}

		if strings.HasSuffix(request.URL.Path, ".jpg") {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("fake-jpeg-bytes"))
			return
		}

		http.NotFound(w, request)
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	client := &multiEngineImageSearchClient{
		httpClient:       server.Client(),
		bingBaseURL:      server.URL + "/bing",
		openverseBaseURL: server.URL + "/v1/images",
		wikimediaBaseURL: server.URL + "/api.php",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := client.search(ctx, "test query", 1, 5)
	if err != nil {
		t.Fatalf("unexpected search error: %v", err)
	}

	if len(res.Items) == 0 {
		t.Fatal("expected search items")
	}

	// Test downloading images
	downloaded := client.downloadImages(ctx, []imageSearchResultItem{
		{
			Title:     "Item 1",
			Thumbnail: server.URL + "/test.jpg",
			URL:       server.URL + "/test.jpg",
		},
	})

	if len(downloaded) != 1 {
		t.Fatalf("expected 1 downloaded item, got %d", len(downloaded))
	}

	if len(downloaded[0].Data) == 0 {
		t.Fatal("expected downloaded image data")
	}

	embeds, files, attachments := buildImageEmbedsAndFiles("test query", downloaded)
	if len(embeds) != 1 || len(files) != 1 || len(attachments) != 1 {
		t.Fatalf("expected 1 embed, 1 file, and 1 attachment, got %d embeds, %d files, %d attachments", len(embeds), len(files), len(attachments))
	}
}
