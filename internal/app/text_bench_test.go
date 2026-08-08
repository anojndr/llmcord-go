package app

import (
	"context"
	"testing"
)

func BenchmarkTruncateRunes(b *testing.B) {
	str := "Hello, World! This is a test string with unicode " +
		"characters like 🚀 and 🤖 to test string rune truncation."

	b.ResetTimer()

	for range b.N {
		_ = truncateRunes(str, 50)
	}
}

func BenchmarkSplitRunesPrefix(b *testing.B) {
	str := "Hello, World! This is a test string with unicode " +
		"characters like 🚀 and 🤖 to test split runes prefix."

	b.ResetTimer()

	for range b.N {
		_, _ = splitRunesPrefix(str, 40)
	}
}

func BenchmarkCompareMessageIDs(b *testing.B) {
	left := "123456789012345678"
	right := "123456789012345679"

	b.ResetTimer()

	for range b.N {
		_ = compareMessageIDs(left, right)
	}
}

func BenchmarkAllowedGeminiDocumentMIMETypes(b *testing.B) {
	b.ResetTimer()

	for range b.N {
		_ = allowedGeminiDocumentMIMETypes()
	}
}

func BenchmarkJoinNonEmpty(b *testing.B) {
	parts := []string{
		"  ",
		"First line of text",
		"",
		"Second line of text",
		"   \n\t ",
		"Third line of text",
	}

	b.ResetTimer()

	for range b.N {
		_ = joinNonEmpty(parts)
	}
}

func BenchmarkRunTasksConcurrently(b *testing.B) {
	ctx := context.Background()

	b.ResetTimer()

	for range b.N {
		_ = runTasksConcurrently(ctx, 4, 10, func(_ context.Context, index int) (int, error) {
			return index * 2, nil
		})
	}
}
