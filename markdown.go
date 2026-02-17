package main

import (
	"regexp"
	"strings"
)

// Telegram MarkdownV2 special characters that must be escaped outside formatting.
const mdv2SpecialChars = `_*[]()~` + "`" + `>#+-=|{}.!`

var (
	// Matches fenced code blocks: ```lang\n...\n```
	fencedCodeRe = regexp.MustCompile("(?s)```([a-zA-Z]*)\\n?(.*?)```")
	// Matches inline code: `...`
	inlineCodeRe = regexp.MustCompile("`([^`]+)`")
	// Matches bold: **text**
	boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)
	// Matches italic with underscores: _text_ (but not inside words)
	italicUnderRe = regexp.MustCompile(`(?:^|(?:\s))_(.+?)_(?:$|(?:\s))`)
	// Matches strikethrough: ~~text~~
	strikeRe = regexp.MustCompile(`~~(.+?)~~`)
	// Matches markdown links: [text](url)
	linkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	// Matches heading lines: # ... ## ... ### ...
	headingRe = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+)$`)
)

// ToTelegramMarkdownV2 converts CommonMark to Telegram MarkdownV2 format.
func ToTelegramMarkdownV2(text string) string {
	// Split input by fenced code blocks to process them separately.
	parts := splitByCodeBlocks(text)

	var result strings.Builder
	for _, part := range parts {
		if part.isCode {
			// Fenced code blocks: escape only backslash and backtick inside.
			lang := part.lang
			code := escapeCodeBlock(part.content)
			result.WriteString("```")
			result.WriteString(lang)
			result.WriteString("\n")
			result.WriteString(code)
			if !strings.HasSuffix(code, "\n") {
				result.WriteString("\n")
			}
			result.WriteString("```")
		} else {
			result.WriteString(convertInlineMarkdown(part.content))
		}
	}
	return result.String()
}

type textPart struct {
	content string
	lang    string
	isCode  bool
}

// splitByCodeBlocks splits text into code and non-code sections.
func splitByCodeBlocks(text string) []textPart {
	var parts []textPart
	matches := fencedCodeRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return []textPart{{content: text, isCode: false}}
	}

	prev := 0
	for _, m := range matches {
		// m[0]:m[1] = full match, m[2]:m[3] = lang, m[4]:m[5] = code content
		if m[0] > prev {
			parts = append(parts, textPart{content: text[prev:m[0]], isCode: false})
		}
		lang := text[m[2]:m[3]]
		code := text[m[4]:m[5]]
		parts = append(parts, textPart{content: code, lang: lang, isCode: true})
		prev = m[1]
	}
	if prev < len(text) {
		parts = append(parts, textPart{content: text[prev:], isCode: false})
	}
	return parts
}

// convertInlineMarkdown converts non-code-block text to MarkdownV2.
func convertInlineMarkdown(text string) string {
	// Process inline code spans first — extract them, convert the rest, re-insert.
	type codeSpan struct {
		placeholder string
		converted   string
	}
	var spans []codeSpan
	counter := 0

	processed := inlineCodeRe.ReplaceAllStringFunc(text, func(match string) string {
		inner := inlineCodeRe.FindStringSubmatch(match)[1]
		placeholder := "\x00ICODE" + strings.Repeat("X", counter) + "\x00"
		counter++
		spans = append(spans, codeSpan{
			placeholder: placeholder,
			converted:   "`" + escapeInlineCode(inner) + "`",
		})
		return placeholder
	})

	// Process links — extract them to protect from escaping.
	type linkSpan struct {
		placeholder string
		converted   string
	}
	var links []linkSpan
	linkCounter := 0

	processed = linkRe.ReplaceAllStringFunc(processed, func(match string) string {
		sub := linkRe.FindStringSubmatch(match)
		linkText := sub[1]
		url := sub[2]
		placeholder := "\x00LINK" + strings.Repeat("X", linkCounter) + "\x00"
		linkCounter++
		// Escape special chars in link text, escape ) and \ in URL
		escapedText := escapeMarkdownV2(linkText)
		escapedURL := strings.ReplaceAll(url, `\`, `\\`)
		escapedURL = strings.ReplaceAll(escapedURL, `)`, `\)`)
		links = append(links, linkSpan{
			placeholder: placeholder,
			converted:   "[" + escapedText + "](" + escapedURL + ")",
		})
		return placeholder
	})

	// Convert headings: # Title -> *Title* (bold)
	processed = headingRe.ReplaceAllStringFunc(processed, func(match string) string {
		sub := headingRe.FindStringSubmatch(match)
		return "*" + sub[2] + "*"
	})

	// Convert bold: **text** -> *text*
	processed = boldRe.ReplaceAllString(processed, "*$1*")

	// Convert strikethrough: ~~text~~ -> ~text~
	processed = strikeRe.ReplaceAllString(processed, "~$1~")

	// Now escape all MarkdownV2 special chars in the non-formatted portions.
	// We need to do this carefully: split by our formatting markers.
	processed = escapePreservingFormatting(processed)

	// Re-insert inline code spans and links.
	for _, s := range spans {
		processed = strings.Replace(processed, escapeMarkdownV2(s.placeholder), s.converted, 1)
	}
	for _, l := range links {
		processed = strings.Replace(processed, escapeMarkdownV2(l.placeholder), l.converted, 1)
	}

	return processed
}

// escapePreservingFormatting escapes special chars but preserves * and ~ used for formatting.
func escapePreservingFormatting(text string) string {
	// We identify bold (*...*) and strikethrough (~...~) spans
	// and escape everything except the formatting markers themselves.
	var result strings.Builder
	runes := []rune(text)
	i := 0

	for i < len(runes) {
		ch := runes[i]

		// Check for formatting spans: *text* or ~text~
		if (ch == '*' || ch == '~') && i+1 < len(runes) {
			marker := ch
			// Find matching close marker
			end := strings.IndexRune(string(runes[i+1:]), marker)
			if end > 0 {
				inner := string(runes[i+1 : i+1+end])
				// Don't treat it as formatting if the inner text is empty or has newlines
				if !strings.Contains(inner, "\n") {
					result.WriteRune(marker)
					result.WriteString(escapeMarkdownV2(inner))
					result.WriteRune(marker)
					i += end + 2
					continue
				}
			}
		}

		// Regular character — escape if special
		if strings.ContainsRune(mdv2SpecialChars, ch) {
			result.WriteRune('\\')
		}
		result.WriteRune(ch)
		i++
	}
	return result.String()
}

// escapeMarkdownV2 escapes all MarkdownV2 special characters.
func escapeMarkdownV2(text string) string {
	var b strings.Builder
	for _, r := range text {
		if strings.ContainsRune(mdv2SpecialChars, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escapeCodeBlock escapes backslash and backtick inside fenced code blocks.
func escapeCodeBlock(text string) string {
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, "`", "\\`")
	return text
}

// escapeInlineCode escapes backslash and backtick inside inline code.
func escapeInlineCode(text string) string {
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, "`", "\\`")
	return text
}
