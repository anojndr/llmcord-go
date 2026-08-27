package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	openverseAPIBaseURL         = "https://api.openverse.org/v1/images/"
	wikimediaAPIBaseURL         = "https://commons.wikimedia.org/w/api.php"
	bingImageSearchBaseURL      = "https://www.bing.com/images/search"
	defaultImageSearchUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	imageSearchTimeout          = 15 * time.Second
	imageDownloadTimeout        = 10 * time.Second
	maxImagesLimit              = 5
	maxImageDownloadBytes       = 8 * 1024 * 1024
)

var (
	bingImageJSONAttrRegex = regexp.MustCompile(`class="iusc"[^>]*m="([^"]+)"`)
	bingImageTagRegex      = regexp.MustCompile(`[\x{e000}-\x{e001}]|<[^>]+>`)
)

type imageSearchResultItem struct {
	Title       string
	URL         string
	Thumbnail   string
	LandingURL  string
	Creator     string
	Source      string
	Data        []byte
	Filename    string
	ContentType string
}

type imageSearchResult struct {
	Query      string
	Page       int
	TotalPages int
	Items      []imageSearchResultItem
}

type imageSearcher interface {
	search(ctx context.Context, query string, page int, pageSize int) (*imageSearchResult, error)
	downloadImages(ctx context.Context, items []imageSearchResultItem) []imageSearchResultItem
}

type multiEngineImageSearchClient struct {
	httpClient       *http.Client
	bingBaseURL      string
	openverseBaseURL string
	wikimediaBaseURL string
}

func newImageSearchClient(httpClient *http.Client) imageSearcher {
	return &multiEngineImageSearchClient{
		httpClient:       httpClient,
		bingBaseURL:      bingImageSearchBaseURL,
		openverseBaseURL: openverseAPIBaseURL,
		wikimediaBaseURL: wikimediaAPIBaseURL,
	}
}

type openverseResponse struct {
	ResultCount int `json:"result_count"`
	PageCount   int `json:"page_count"`
	Results     []struct {
		Title             string `json:"title"`
		URL               string `json:"url"`
		Thumbnail         string `json:"thumbnail"`
		ForeignLandingURL string `json:"foreign_landing_url"`
		Creator           string `json:"creator"`
		Source            string `json:"source"`
	} `json:"results"`
}

type wikimediaResponse struct {
	Query struct {
		Pages map[string]struct {
			Title     string `json:"title"`
			ImageInfo []struct {
				URL            string `json:"url"`
				ThumbURL       string `json:"thumburl"`
				DescriptionURL string `json:"descriptionurl"`
				MIME           string `json:"mime"`
			} `json:"imageinfo"`
		} `json:"pages"`
	} `json:"query"`
}

func (client *multiEngineImageSearchClient) search(
	ctx context.Context,
	query string,
	page int,
	pageSize int,
) (*imageSearchResult, error) {
	trimmedQuery := strings.TrimSpace(query)
	if trimmedQuery == "" {
		return &imageSearchResult{
			Query:      query,
			Page:       page,
			TotalPages: 0,
			Items:      nil,
		}, nil
	}

	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > maxImagesLimit {
		pageSize = maxImagesLimit
	}

	// 1. Try Bing Web Image Search (free, high quality, broad coverage, no key needed)
	bingResult, bingErr := client.searchBing(ctx, trimmedQuery, page, pageSize)
	if bingErr == nil && bingResult != nil && len(bingResult.Items) > 0 {
		if len(bingResult.Items) > maxImagesLimit {
			bingResult.Items = bingResult.Items[:maxImagesLimit]
		}
		return bingResult, nil
	}

	// 2. Try Openverse
	ovResult, ovErr := client.searchOpenverse(ctx, trimmedQuery, page, pageSize)
	if ovErr == nil && ovResult != nil && len(ovResult.Items) > 0 {
		if len(ovResult.Items) > maxImagesLimit {
			ovResult.Items = ovResult.Items[:maxImagesLimit]
		}
		return ovResult, nil
	}

	// 3. Fallback to Wikimedia Commons
	wmResult, wmErr := client.searchWikimedia(ctx, trimmedQuery, page, pageSize)
	if wmErr == nil && wmResult != nil && len(wmResult.Items) > 0 {
		if len(wmResult.Items) > maxImagesLimit {
			wmResult.Items = wmResult.Items[:maxImagesLimit]
		}
		return wmResult, nil
	}

	if bingErr != nil {
		return nil, bingErr
	}
	if ovErr != nil {
		return nil, ovErr
	}
	if wmErr != nil {
		return nil, wmErr
	}

	return &imageSearchResult{
		Query:      trimmedQuery,
		Page:       page,
		TotalPages: 0,
		Items:      nil,
	}, nil
}

func (client *multiEngineImageSearchClient) searchBing(
	ctx context.Context,
	query string,
	page int,
	pageSize int,
) (*imageSearchResult, error) {
	baseURL := client.bingBaseURL
	if baseURL == "" {
		baseURL = bingImageSearchBaseURL
	}

	firstIndex := (page-1)*pageSize + 1
	reqURL := fmt.Sprintf("%s?q=%s&form=HDRSC2&first=%d", baseURL, url.QueryEscape(query), firstIndex)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create bing image request: %w", err)
	}

	req.Header.Set("User-Agent", defaultImageSearchUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	httpClient := client.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform bing image request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bing image request returned status: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read bing image response: %w", err)
	}

	htmlContent := string(bodyBytes)
	matches := bingImageJSONAttrRegex.FindAllStringSubmatch(htmlContent, -1)
	if len(matches) == 0 {
		// Try fallback regex if class="iusc" format differs
		altRegex := regexp.MustCompile(`m="?(\{&quot;murl&quot;:[^\"]+\})"?`)
		matches = altRegex.FindAllStringSubmatch(htmlContent, -1)
	}

	items := make([]imageSearchResultItem, 0, pageSize)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}

		rawJSON := html.UnescapeString(m[1])
		var entry struct {
			MURL string `json:"murl"`
			TURL string `json:"turl"`
			PURL string `json:"purl"`
			T1   string `json:"t1"`
			Desc string `json:"desc"`
		}

		if err := json.Unmarshal([]byte(rawJSON), &entry); err != nil {
			continue
		}

		imgURL := strings.TrimSpace(entry.MURL)
		if imgURL == "" {
			continue
		}

		thumbURL := strings.TrimSpace(entry.TURL)
		if thumbURL == "" {
			thumbURL = imgURL
		}

		landingURL := strings.TrimSpace(entry.PURL)
		if landingURL == "" {
			landingURL = imgURL
		}

		rawTitle := strings.TrimSpace(entry.T1)
		if rawTitle == "" {
			rawTitle = strings.TrimSpace(entry.Desc)
		}
		if rawTitle == "" {
			rawTitle = query
		}

		cleanTitle := cleanImageTitle(rawTitle)

		items = append(items, imageSearchResultItem{
			Title:      cleanTitle,
			URL:        imgURL,
			Thumbnail:  thumbURL,
			LandingURL: landingURL,
			Creator:    "",
			Source:     "Bing",
		})

		if len(items) >= pageSize {
			break
		}
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("no bing images parsed for query %q", query)
	}

	return &imageSearchResult{
		Query:      query,
		Page:       page,
		TotalPages: 1,
		Items:      items,
	}, nil
}

func cleanImageTitle(title string) string {
	cleaned := bingImageTagRegex.ReplaceAllString(title, "")
	cleaned = html.UnescapeString(cleaned)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	return strings.TrimSpace(cleaned)
}

func (client *multiEngineImageSearchClient) searchOpenverse(
	ctx context.Context,
	query string,
	page int,
	pageSize int,
) (*imageSearchResult, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("page", strconv.Itoa(page))
	params.Set("page_size", strconv.Itoa(pageSize))

	baseURL := client.openverseBaseURL
	if baseURL == "" {
		baseURL = openverseAPIBaseURL
	}
	reqURL := baseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create openverse request: %w", err)
	}

	req.Header.Set("User-Agent", defaultImageSearchUserAgent)
	req.Header.Set("Accept", "application/json")

	httpClient := client.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform openverse request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openverse request failed with status: %d", resp.StatusCode)
	}

	var parsed openverseResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode openverse response: %w", err)
	}

	items := make([]imageSearchResultItem, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		thumb := strings.TrimSpace(r.Thumbnail)
		if thumb == "" {
			thumb = strings.TrimSpace(r.URL)
		}
		orig := strings.TrimSpace(r.URL)
		landing := strings.TrimSpace(r.ForeignLandingURL)
		if landing == "" {
			landing = orig
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = "Untitled Image"
		}

		source := strings.TrimSpace(r.Source)
		if source == "" {
			source = "Openverse"
		}

		items = append(items, imageSearchResultItem{
			Title:      title,
			URL:        orig,
			Thumbnail:  thumb,
			LandingURL: landing,
			Creator:    strings.TrimSpace(r.Creator),
			Source:     source,
		})
	}

	totalPages := parsed.PageCount
	if totalPages <= 0 && len(items) > 0 {
		totalPages = 1
	}

	return &imageSearchResult{
		Query:      query,
		Page:       page,
		TotalPages: totalPages,
		Items:      items,
	}, nil
}

func (client *multiEngineImageSearchClient) searchWikimedia(
	ctx context.Context,
	query string,
	page int,
	pageSize int,
) (*imageSearchResult, error) {
	offset := (page - 1) * pageSize
	params := url.Values{}
	params.Set("action", "query")
	params.Set("generator", "search")
	params.Set("gsrsearch", query)
	params.Set("gsrnamespace", "6") // File: namespace
	params.Set("gsroffset", strconv.Itoa(offset))
	params.Set("gsrlimit", strconv.Itoa(pageSize))
	params.Set("prop", "imageinfo")
	params.Set("iiprop", "url|extmetadata|mime")
	params.Set("iiurlwidth", "800")
	params.Set("format", "json")
	params.Set("origin", "*")

	baseURL := client.wikimediaBaseURL
	if baseURL == "" {
		baseURL = wikimediaAPIBaseURL
	}
	reqURL := baseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create wikimedia request: %w", err)
	}

	req.Header.Set("User-Agent", defaultImageSearchUserAgent)
	req.Header.Set("Accept", "application/json")

	httpClient := client.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform wikimedia request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wikimedia request failed with status: %d", resp.StatusCode)
	}

	var parsed wikimediaResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode wikimedia response: %w", err)
	}

	items := make([]imageSearchResultItem, 0, len(parsed.Query.Pages))
	for _, p := range parsed.Query.Pages {
		if len(p.ImageInfo) == 0 {
			continue
		}
		info := p.ImageInfo[0]
		mime := strings.ToLower(strings.TrimSpace(info.MIME))
		if mime != "" && !strings.HasPrefix(mime, "image/") {
			continue
		}

		thumb := strings.TrimSpace(info.ThumbURL)
		orig := strings.TrimSpace(info.URL)
		if thumb == "" {
			thumb = orig
		}
		if orig == "" {
			orig = thumb
		}
		landing := strings.TrimSpace(info.DescriptionURL)
		if landing == "" {
			landing = orig
		}

		title := strings.TrimSpace(p.Title)
		title = strings.TrimPrefix(title, "File:")
		title = strings.TrimPrefix(title, "file:")
		title = strings.TrimSpace(title)
		if title == "" {
			title = "Wikimedia Commons Image"
		}

		items = append(items, imageSearchResultItem{
			Title:      title,
			URL:        orig,
			Thumbnail:  thumb,
			LandingURL: landing,
			Creator:    "",
			Source:     "Wikimedia Commons",
		})
	}

	totalPages := 1
	if len(items) == pageSize {
		totalPages = page + 1
	} else if page > 1 {
		totalPages = page
	}

	return &imageSearchResult{
		Query:      query,
		Page:       page,
		TotalPages: totalPages,
		Items:      items,
	}, nil
}

func (client *multiEngineImageSearchClient) downloadImages(
	ctx context.Context,
	items []imageSearchResultItem,
) []imageSearchResultItem {
	if len(items) == 0 {
		return nil
	}

	if len(items) > maxImagesLimit {
		items = items[:maxImagesLimit]
	}

	results := make([]imageSearchResultItem, len(items))
	copy(results, items)

	httpClient := client.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			item := &results[idx]

			downloadURL := item.Thumbnail
			if strings.TrimSpace(downloadURL) == "" {
				downloadURL = item.URL
			}
			if strings.TrimSpace(downloadURL) == "" {
				return
			}

			reqCtx, cancel := context.WithTimeout(ctx, imageDownloadTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, downloadURL, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", defaultImageSearchUserAgent)
			req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")

			resp, err := httpClient.Do(req)
			if err != nil {
				// If thumbnail failed and URL is different, try full URL
				if item.URL != "" && item.URL != downloadURL {
					req2, err2 := http.NewRequestWithContext(reqCtx, http.MethodGet, item.URL, nil)
					if err2 == nil {
						req2.Header.Set("User-Agent", defaultImageSearchUserAgent)
						req2.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
						resp2, err3 := httpClient.Do(req2)
						if err3 == nil {
							resp = resp2
							downloadURL = item.URL
							err = nil
						}
					}
				}
			}

			if err != nil || resp == nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return
			}

			data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageDownloadBytes))
			if err != nil || len(data) == 0 {
				return
			}

			contentType := resp.Header.Get("Content-Type")
			ext := imageExtensionFromContentType(contentType)
			if ext == "" {
				ext = strings.ToLower(path.Ext(downloadURL))
				if ext == "" || len(ext) > 5 {
					ext = ".jpg"
				}
			}

			filename := fmt.Sprintf("image_%d%s", idx+1, ext)
			item.Data = data
			item.Filename = filename
			item.ContentType = contentType
		}(i)
	}

	wg.Wait()
	return results
}

func imageExtensionFromContentType(contentType string) string {
	lower := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case strings.HasPrefix(lower, "image/jpeg"), strings.HasPrefix(lower, "image/jpg"):
		return ".jpg"
	case strings.HasPrefix(lower, "image/png"):
		return ".png"
	case strings.HasPrefix(lower, "image/webp"):
		return ".webp"
	case strings.HasPrefix(lower, "image/gif"):
		return ".gif"
	case strings.HasPrefix(lower, "image/svg"):
		return ".svg"
	default:
		return ""
	}
}

func buildImageEmbedsAndFiles(
	query string,
	items []imageSearchResultItem,
) ([]*discordgo.MessageEmbed, []*discordgo.File, []*discordgo.MessageAttachment) {
	if len(items) == 0 {
		return nil, nil, nil
	}

	if len(items) > maxImagesLimit {
		items = items[:maxImagesLimit]
	}

	embeds := make([]*discordgo.MessageEmbed, 0, len(items))
	files := make([]*discordgo.File, 0, len(items))
	attachments := make([]*discordgo.MessageAttachment, 0, len(items))

	for i, item := range items {
		embed := new(discordgo.MessageEmbed)
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = fmt.Sprintf("Image %d", i+1)
		}
		if len(title) > 250 {
			title = title[:247] + "..."
		}
		embed.Title = title
		if strings.TrimSpace(item.LandingURL) != "" {
			embed.URL = item.LandingURL
		}

		if strings.TrimSpace(item.Source) != "" {
			embed.Footer = &discordgo.MessageEmbedFooter{
				Text: fmt.Sprintf("Source: %s • Image %d/%d", item.Source, i+1, len(items)),
			}
		} else {
			embed.Footer = &discordgo.MessageEmbedFooter{
				Text: fmt.Sprintf("Image %d/%d", i+1, len(items)),
			}
		}

		if len(item.Data) > 0 && item.Filename != "" {
			fileIndex := len(files)
			files = append(files, &discordgo.File{
				Name:        item.Filename,
				ContentType: item.ContentType,
				Reader:      bytes.NewReader(item.Data),
			})
			attachments = append(attachments, &discordgo.MessageAttachment{
				ID:       strconv.Itoa(fileIndex),
				Filename: item.Filename,
			})
			embed.Image = &discordgo.MessageEmbedImage{
				URL: "attachment://" + item.Filename,
			}
		} else {
			imageURL := strings.TrimSpace(item.Thumbnail)
			if imageURL == "" {
				imageURL = strings.TrimSpace(item.URL)
			}
			if imageURL != "" {
				embed.Image = &discordgo.MessageEmbedImage{
					URL: imageURL,
				}
			}
		}

		embeds = append(embeds, embed)
	}

	return embeds, files, attachments
}
