package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/chromedp/chromedp"
)

const (
	rentryBrowserCDPTimeout       = 60 * time.Second
	rentryBrowserFormSelector     = "#entry-form"
	rentryBrowserTextareaSelector = "#id_text"
	rentryBrowserSubmitSelector   = "#submitButton"
	rentryBrowserLocationPollWait = 100 * time.Millisecond
)

var errRentryBrowserEmptyEndpoint = errors.New("create Rentry entry in browser: empty endpoint")
var errRentryBrowserEditorMissing = errors.New("set Rentry editor text: CodeMirror editor not found")

// chromedpRentryBrowserCreator submits the Rentry form through a real headless
// Chrome instance so Cloudflare's managed challenge is solved by the browser
// itself. The plain-HTTP path in httpRentryClient cannot pass that challenge,
// which is why the fallback exists.
type chromedpRentryBrowserCreator struct {
	execAllocatorOptions []chromedp.ExecAllocatorOption
	browserPath          string
}

func newChromedpRentryBrowserCreator(browserPath string) *chromedpRentryBrowserCreator {
	return &chromedpRentryBrowserCreator{
		execAllocatorOptions: make([]chromedp.ExecAllocatorOption, 0),
		browserPath:          strings.TrimSpace(browserPath),
	}
}

func (creator *chromedpRentryBrowserCreator) createEntry(
	ctx context.Context,
	endpoint string,
	text string,
) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", errRentryBrowserEmptyEndpoint
	}

	options := make([]chromedp.ExecAllocatorOption, 0, len(chromedp.DefaultExecAllocatorOptions)+1)
	options = append(options, chromedp.DefaultExecAllocatorOptions[:]...)

	if creator.browserPath != "" {
		options = append(options, chromedp.ExecPath(creator.browserPath))
	}

	allocatorContext, cancelAllocator := chromedp.NewExecAllocator(ctx, options...)
	defer cancelAllocator()

	browserContext, cancelBrowser := chromedp.NewContext(allocatorContext)
	defer cancelBrowser()

	timeoutContext, cancelTimeout := context.WithTimeout(browserContext, rentryBrowserCDPTimeout)
	defer cancelTimeout()

	var entryURL string

	err := chromedp.Run(timeoutContext,
		chromedp.Navigate(endpoint),

		// The Rentry editor is a CodeMirror instance layered over a hidden
		// <textarea id="id_text">. Setting the CodeMirror value keeps the
		// hidden textarea in sync; typing into the contenteditable is
		// unnecessary and fragile. The submit button then posts the form
		// through the browser, where Cloudflare's checks pass.
		chromedp.WaitReady(rentryBrowserFormSelector),

		creator.setEditorTextAction(text),
		chromedp.Click(rentryBrowserSubmitSelector, chromedp.NodeVisible),
		creator.waitForEntryURLAction(endpoint, &entryURL),
	)
	if err != nil {
		return "", fmt.Errorf("create Rentry entry in browser: %w", err)
	}

	return entryURL, nil
}

func (creator *chromedpRentryBrowserCreator) setEditorTextAction(text string) chromedp.ActionFunc {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var editorSet bool

		err := chromedp.Evaluate(
			`(() => {
				const textarea = document.querySelector('`+rentryBrowserTextareaSelector+`');
				if (!textarea || !textarea.nextElementSibling ||
					!textarea.nextElementSibling.CodeMirror) {
					return false;
				}
				textarea.nextElementSibling.CodeMirror.setValue(`+jsString(text)+`);
				return true;
			})()`,
			&editorSet,
		).Do(ctx)
		if err != nil {
			return fmt.Errorf("set Rentry editor text: %w", err)
		}

		if !editorSet {
			return errRentryBrowserEditorMissing
		}

		return nil
	})
}

func (creator *chromedpRentryBrowserCreator) waitForEntryURLAction(
	endpoint string,
	entryURL *string,
) chromedp.ActionFunc {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for {
			ctxErr := ctx.Err()
			if ctxErr != nil {
				return fmt.Errorf("wait for Rentry entry URL: %w", ctxErr)
			}

			var location string

			locationErr := chromedp.Location(&location).Do(ctx)
			if locationErr != nil {
				return fmt.Errorf("read Rentry browser location: %w", locationErr)
			}

			location = strings.TrimSuffix(location, "/")
			endpointTrimmed := strings.TrimSuffix(endpoint, "/")

			if location != endpointTrimmed {
				*entryURL = location

				return nil
			}

			time.Sleep(rentryBrowserLocationPollWait)
		}
	})
}

// jsString wraps s as a single-quoted JavaScript string literal, escaping
// backslashes, single quotes, line/control characters, and anything else that
// would not be valid inside a JS literal, so arbitrary markdown can be injected
// into an Evaluate call safely.
func jsString(s string) string {
	var builder strings.Builder

	builder.WriteByte('\'')

	for _, character := range s {
		switch character {
		case '\\':
			builder.WriteString(`\\`)
		case '\'':
			builder.WriteString(`\'`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if unicode.IsControl(character) {
				builder.WriteString(strconv.QuoteRune(character))
			} else {
				builder.WriteRune(character)
			}
		}
	}

	builder.WriteByte('\'')

	return builder.String()
}
