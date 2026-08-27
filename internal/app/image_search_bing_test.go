package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBingImageSearchParser(t *testing.T) {
	t.Parallel()

	htmlResponse := `<!DOCTYPE html><html><body>
	<div class="imgpt">
		<a class="iusc" m="{&quot;murl&quot;:&quot;https://example.com/photo1.jpg&quot;,&quot;turl&quot;:&quot;https://example.com/thumb1.jpg&quot;,&quot;purl&quot;:&quot;https://example.com/page1&quot;,&quot;t1&quot;:&quot;&#xe000;Wakana&#xe001; &#xe000;Aoi&#xe001; Photo 1&quot;}"></a>
		<a class="iusc" m="{&quot;murl&quot;:&quot;https://example.com/photo2.jpg&quot;,&quot;turl&quot;:&quot;https://example.com/thumb2.jpg&quot;,&quot;purl&quot;:&quot;https://example.com/page2&quot;,&quot;t1&quot;:&quot;Wakana Aoi Portrait 2&quot;}"></a>
		<a class="iusc" m="{&quot;murl&quot;:&quot;https://example.com/photo3.jpg&quot;,&quot;turl&quot;:&quot;https://example.com/thumb3.jpg&quot;,&quot;purl&quot;:&quot;https://example.com/page3&quot;,&quot;t1&quot;:&quot;Wakana Aoi Event 3&quot;}"></a>
		<a class="iusc" m="{&quot;murl&quot;:&quot;https://example.com/photo4.jpg&quot;,&quot;turl&quot;:&quot;https://example.com/thumb4.jpg&quot;,&quot;purl&quot;:&quot;https://example.com/page4&quot;,&quot;t1&quot;:&quot;Wakana Aoi Drama 4&quot;}"></a>
		<a class="iusc" m="{&quot;murl&quot;:&quot;https://example.com/photo5.jpg&quot;,&quot;turl&quot;:&quot;https://example.com/thumb5.jpg&quot;,&quot;purl&quot;:&quot;https://example.com/page5&quot;,&quot;t1&quot;:&quot;Wakana Aoi Movie 5&quot;}"></a>
		<a class="iusc" m="{&quot;murl&quot;:&quot;https://example.com/photo6.jpg&quot;,&quot;turl&quot;:&quot;https://example.com/thumb6.jpg&quot;,&quot;purl&quot;:&quot;https://example.com/page6&quot;,&quot;t1&quot;:&quot;Wakana Aoi Extra 6&quot;}"></a>
	</div>
	</body></html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(htmlResponse))
	}))
	defer server.Close()

	client := &multiEngineImageSearchClient{
		httpClient:  server.Client(),
		bingBaseURL: server.URL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := client.search(ctx, "wakana aoi", 1, 5)
	if err != nil {
		t.Fatalf("unexpected search error: %v", err)
	}

	if len(result.Items) != 5 {
		t.Fatalf("expected 5 items, got %d", len(result.Items))
	}

	if result.Items[0].Title != "Wakana Aoi Photo 1" {
		t.Fatalf("expected cleaned title %q, got %q", "Wakana Aoi Photo 1", result.Items[0].Title)
	}

	if result.Items[0].URL != "https://example.com/photo1.jpg" {
		t.Fatalf("unexpected image URL: %q", result.Items[0].URL)
	}

	if result.Items[0].Thumbnail != "https://example.com/thumb1.jpg" {
		t.Fatalf("unexpected thumbnail URL: %q", result.Items[0].Thumbnail)
	}

	if result.Items[0].LandingURL != "https://example.com/page1" {
		t.Fatalf("unexpected landing URL: %q", result.Items[0].LandingURL)
	}
}
