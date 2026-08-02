package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestWebsiteClientFetchExtractsAliExpressEmbeddedProductMetadata(t *testing.T) {
	t.Parallel()

	const productURL = "https://www.aliexpress.com/item/1005012332726528.html"

	htmlBody := strings.Join([]string{
		"<!doctype html>",
		"<html><head><title>AliExpress - Online Shopping</title>",
		`<meta name="description" content="">`,
		`<meta property="og:title" content="Precision Mouse Skates - AliExpress 7">`,
		`<meta property="og:image" content="//cdn.example.com/primary.jpg">`,
		"<script>",
		`window._d_c_.DCData = {"extParams":{"note":"brace } and escaped \"quote\""},`,
		`"imagePathList":["https://cdn.example.com/primary.jpg",`,
		`"https://cdn.example.com/secondary.jpg"]};`,
		"</script></head>",
		"<body><main>AliExpress global navigation shell without product details.</main></body></html>",
	}, "")

	httpClient := new(http.Client)
	httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != productURL {
			t.Fatalf("unexpected request url: %q", request.URL.String())

			return nil, os.ErrInvalid
		}

		return newWebsiteTestResponse(
			http.StatusOK,
			http.Header{contentTypeHeader: []string{"text/html; charset=utf-8"}},
			htmlBody,
			request,
		), nil
	})

	client := newWebsiteTestClient(httpClient, defaultExaContentsEndpoint, defaultTavilyExtractEndpoint)

	result, err := client.fetch(context.Background(), testSearchConfig(), productURL)
	if err != nil {
		t.Fatalf("fetch AliExpress product metadata: %v", err)
	}

	if result.Title != "Precision Mouse Skates" {
		t.Fatalf("unexpected title: %q", result.Title)
	}

	for _, expectedText := range []string{
		"Product ID: 1005012332726528",
		"Product name: Precision Mouse Skates",
		"https://cdn.example.com/primary.jpg",
		"https://cdn.example.com/secondary.jpg",
	} {
		if !strings.Contains(result.Content, expectedText) {
			t.Fatalf("expected %q in product content: %q", expectedText, result.Content)
		}
	}

	if strings.Contains(result.Content, "global navigation shell") {
		t.Fatalf("unexpected shell text in product content: %q", result.Content)
	}
}

func TestAliExpressProductID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		pageURL   string
		expected  string
		extracted bool
	}{
		{
			name:      "global product",
			pageURL:   "https://www.aliexpress.com/item/1005012332726528.html?source=test",
			expected:  "1005012332726528",
			extracted: true,
		},
		{
			name:      "United States product",
			pageURL:   "https://www.aliexpress.us/item/3256812146411776.htm",
			expected:  "3256812146411776",
			extracted: true,
		},
		{
			name:      "non product",
			pageURL:   "https://www.aliexpress.com/store/1984274",
			expected:  "",
			extracted: false,
		},
		{
			name:      "different host",
			pageURL:   "https://example.com/item/1005012332726528.html",
			expected:  "",
			extracted: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			productID, extracted := aliExpressProductID(testCase.pageURL)
			if productID != testCase.expected || extracted != testCase.extracted {
				t.Fatalf(
					"aliExpressProductID(%q) = (%q, %t), want (%q, %t)",
					testCase.pageURL,
					productID,
					extracted,
					testCase.expected,
					testCase.extracted,
				)
			}
		})
	}
}
