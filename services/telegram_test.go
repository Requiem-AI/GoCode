package services

import "testing"

func TestEscapeMarkdownV2_PlainText(t *testing.T) {
	got := escapeMarkdownV2("hello world")
	want := "hello world"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_Empty(t *testing.T) {
	got := escapeMarkdownV2("")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestEscapeMarkdownV2_SpecialChars(t *testing.T) {
	// Every special char in normal text must be escaped.
	tests := []struct {
		in, want string
	}{
		{"_", "\\_"},
		{"*", "\\*"},
		{"[", "\\["},
		{"]", "\\]"},
		{"(", "\\("},
		{")", "\\)"},
		{"~", "\\~"},
		{"`", "\\`"}, // lone backtick, no closing match
		{">", "\\>"},
		{"#", "\\#"},
		{"+", "\\+"},
		{"-", "\\-"},
		{"=", "\\="},
		{"|", "\\|"},
		{"{", "\\{"},
		{"}", "\\}"},
		{".", "\\."},
		{"!", "\\!"},
		{`\`, `\\`},
	}
	for _, tc := range tests {
		got := escapeMarkdownV2(tc.in)
		if got != tc.want {
			t.Errorf("escapeMarkdownV2(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapeMarkdownV2_MixedText(t *testing.T) {
	got := escapeMarkdownV2("hello_world! version 2.0")
	want := `hello\_world\! version 2\.0`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_InlineCode(t *testing.T) {
	// Inside inline code only ` and \ are escaped.
	got := escapeMarkdownV2("`hello_world*`")
	want := "`hello_world*`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_InlineCodeWithBackslash(t *testing.T) {
	got := escapeMarkdownV2("`path\\to\\file`")
	want := "`path\\\\to\\\\file`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_FencedCodeBlock(t *testing.T) {
	// Inside fenced code blocks only ` and \ are escaped. Special chars are preserved.
	input := "```\nfoo_bar*baz\n```"
	got := escapeMarkdownV2(input)
	want := "```\nfoo_bar*baz\n```"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_FencedCodeBlockWithLang(t *testing.T) {
	input := "```go\nfunc main() {}\n```"
	got := escapeMarkdownV2(input)
	want := "```go\nfunc main() {}\n```"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_FencedCodeBlockWithBackslash(t *testing.T) {
	input := "```\npath\\to\\file\n```"
	got := escapeMarkdownV2(input)
	want := "```\npath\\\\to\\\\file\n```"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_Link(t *testing.T) {
	input := "[click here](https://example.com)"
	got := escapeMarkdownV2(input)
	want := "[click here](https://example.com)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_LinkWithSpecialTextChars(t *testing.T) {
	input := "[hello_world!](https://example.com)"
	got := escapeMarkdownV2(input)
	want := `[hello\_world\!](https://example.com)`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_LinkWithParenInURL(t *testing.T) {
	input := "[wiki](https://en.wikipedia.org/wiki/Go_(language))"
	got := escapeMarkdownV2(input)
	// The inner ) is escaped, outer ) closes the link
	want := `[wiki](https://en.wikipedia.org/wiki/Go_(language\))`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_LinkWithBackslashInURL(t *testing.T) {
	input := `[file](https://example.com/a\b)`
	got := escapeMarkdownV2(input)
	want := `[file](https://example.com/a\\b)`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_BrokenLink_NoCloseBracket(t *testing.T) {
	input := "[hello world"
	got := escapeMarkdownV2(input)
	want := "\\[hello world"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_BrokenLink_NoParen(t *testing.T) {
	input := "[hello]world"
	got := escapeMarkdownV2(input)
	want := "\\[hello\\]world"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_UnmatchedBacktick(t *testing.T) {
	input := "it`s a test"
	got := escapeMarkdownV2(input)
	// The ` matches the later... wait, there's no closing `. But there IS no
	// second backtick, so it should be escaped.
	// Actually "it`s a test" has no second backtick so ` is unmatched.
	want := "it\\`s a test"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_UnmatchedTripleBacktick(t *testing.T) {
	input := "```no closing fence"
	got := escapeMarkdownV2(input)
	want := "\\`\\`\\`no closing fence"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_MixedContent(t *testing.T) {
	input := "Hello! Check `config.yaml` and [docs](https://example.com/path)."
	got := escapeMarkdownV2(input)
	want := "Hello\\! Check `config.yaml` and [docs](https://example.com/path)\\."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_CodeBlockThenText(t *testing.T) {
	input := "```\ncode_here\n```\nDone!"
	got := escapeMarkdownV2(input)
	want := "```\ncode_here\n```\nDone\\!"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_ConsecutiveSpecialChars(t *testing.T) {
	got := escapeMarkdownV2("**bold**")
	want := "\\*\\*bold\\*\\*"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeMarkdownV2_NumberedList(t *testing.T) {
	got := escapeMarkdownV2("1. first\n2. second")
	want := "1\\. first\n2\\. second"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
