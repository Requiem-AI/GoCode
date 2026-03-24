package services

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestParseCommandText_Basic(t *testing.T) {
	cmd, payload, ok := parseCommandText("/clear")
	if !ok {
		t.Fatalf("expected command parse to succeed")
	}
	if cmd != "/clear" {
		t.Fatalf("cmd = %q, want %q", cmd, "/clear")
	}
	if payload != "" {
		t.Fatalf("payload = %q, want empty", payload)
	}
}

func TestParseCommandText_WithMentionAndPayload(t *testing.T) {
	cmd, payload, ok := parseCommandText("/new@gocode_sh_bot my-topic https://github.com/org/repo")
	if !ok {
		t.Fatalf("expected command parse to succeed")
	}
	if cmd != "/new" {
		t.Fatalf("cmd = %q, want %q", cmd, "/new")
	}
	if payload != "my-topic https://github.com/org/repo" {
		t.Fatalf("payload = %q, want %q", payload, "my-topic https://github.com/org/repo")
	}
}

func TestParseCommandText_RejectsNonCommand(t *testing.T) {
	if _, _, ok := parseCommandText("hello world"); ok {
		t.Fatalf("expected parse to fail for non-command text")
	}
}

func TestParseCommandText_GitWithPayload(t *testing.T) {
	cmd, payload, ok := parseCommandText("/git status --short")
	if !ok {
		t.Fatalf("expected command parse to succeed")
	}
	if cmd != "/git" {
		t.Fatalf("cmd = %q, want %q", cmd, "/git")
	}
	if payload != "status --short" {
		t.Fatalf("payload = %q, want %q", payload, "status --short")
	}
}

func TestTruncateTelegramText(t *testing.T) {
	in := ""
	for i := 0; i < 5000; i++ {
		in += "a"
	}

	got := truncateTelegramText(in)
	if len(got) > 3900+len("\n\n[output truncated]") {
		t.Fatalf("unexpected output length: %d", len(got))
	}
	if got[len(got)-len("[output truncated]"):] != "[output truncated]" {
		t.Fatalf("expected truncated suffix in output")
	}
}

func TestProcessRunQueue_SerializesTasks(t *testing.T) {
	svc := &TelegramService{
		runQueues: make(map[string]chan func()),
	}

	const tasks = 8
	queue := make(chan func(), 64)
	svc.runQueues["test"] = queue
	go svc.processRunQueue("test", queue)

	var inFlight int32
	var maxInFlight int32
	done := make(chan struct{}, tasks)

	for i := 0; i < tasks; i++ {
		queue <- func() {
			current := atomic.AddInt32(&inFlight, 1)
			for {
				seen := atomic.LoadInt32(&maxInFlight)
				if current <= seen || atomic.CompareAndSwapInt32(&maxInFlight, seen, current) {
					break
				}
			}

			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			done <- struct{}{}
		}
	}

	for i := 0; i < tasks; i++ {
		<-done
	}

	if maxInFlight != 1 {
		t.Fatalf("expected max in-flight tasks = 1, got %d", maxInFlight)
	}
}

type testNetError struct {
	timeout   bool
	temporary bool
}

func (e testNetError) Error() string   { return "network error" }
func (e testNetError) Timeout() bool   { return e.timeout }
func (e testNetError) Temporary() bool { return e.temporary }

func TestIsRetryableTelegramSendError_NetworkTimeout(t *testing.T) {
	err := testNetError{timeout: true}
	if !isRetryableTelegramSendError(err) {
		t.Fatalf("expected network timeout to be retryable")
	}
}

func TestIsRetryableTelegramSendError_TooManyRequests(t *testing.T) {
	err := errors.New("telegram: Too Many Requests: retry after 1")
	if !isRetryableTelegramSendError(err) {
		t.Fatalf("expected 429 to be retryable")
	}
}

func TestIsRetryableTelegramSendError_ParseEntities(t *testing.T) {
	err := errors.New("telegram: Bad Request: can't parse entities: Character '.' is reserved and must be escaped")
	if isRetryableTelegramSendError(err) {
		t.Fatalf("expected parse entities error to be non-retryable")
	}
}

func TestSplitMessage_Short(t *testing.T) {
	chunks := splitMessage("hello", 4096)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Fatalf("expected single chunk, got %d: %v", len(chunks), chunks)
	}
}

func TestSplitMessage_ExactLimit(t *testing.T) {
	text := strings.Repeat("a", 4096)
	chunks := splitMessage(text, 4096)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestSplitMessage_SplitsAtNewline(t *testing.T) {
	line := strings.Repeat("a", 2000)
	text := line + "\n" + line + "\n" + line
	chunks := splitMessage(text, 4096)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != line+"\n"+line {
		t.Errorf("chunk[0] unexpected: len=%d", len(chunks[0]))
	}
	if chunks[1] != line {
		t.Errorf("chunk[1] unexpected: len=%d", len(chunks[1]))
	}
}

func TestSplitMessage_SplitsAtSpace(t *testing.T) {
	// One long line with spaces
	word := strings.Repeat("a", 100)
	var parts []string
	for i := 0; i < 50; i++ {
		parts = append(parts, word)
	}
	text := strings.Join(parts, " ") // 50*100 + 49 spaces = 5049
	chunks := splitMessage(text, 4096)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 4096 {
			t.Errorf("chunk[%d] exceeds limit: len=%d", i, len(c))
		}
	}
}

func TestSplitMessage_HardSplit(t *testing.T) {
	// No spaces or newlines
	text := strings.Repeat("a", 5000)
	chunks := splitMessage(text, 4096)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if len(chunks[0]) != 4096 {
		t.Errorf("chunk[0] len = %d, want 4096", len(chunks[0]))
	}
	if len(chunks[1]) != 904 {
		t.Errorf("chunk[1] len = %d, want 904", len(chunks[1]))
	}
}

func TestSplitMessage_Empty(t *testing.T) {
	chunks := splitMessage("", 4096)
	if len(chunks) != 1 || chunks[0] != "" {
		t.Fatalf("expected single empty chunk, got %d: %v", len(chunks), chunks)
	}
}

func TestDetectFileURIs_Basic(t *testing.T) {
	text := "Created output file://reports/summary.txt and file:///tmp/build.log"
	files := detectFileURIs(text)
	if len(files) != 2 {
		t.Fatalf("expected 2 detected file URIs, got %d", len(files))
	}
	if files[0].Raw != "file://reports/summary.txt" || files[0].Path != "reports/summary.txt" {
		t.Fatalf("unexpected first file: %+v", files[0])
	}
	if files[1].Raw != "file:///tmp/build.log" || files[1].Path != "/tmp/build.log" {
		t.Fatalf("unexpected second file: %+v", files[1])
	}
}

func TestDetectFileURIs_DeduplicatesAndTrims(t *testing.T) {
	text := "Use [artifact](file://out/app.tar.gz). Also file://out/app.tar.gz!"
	files := detectFileURIs(text)
	if len(files) != 1 {
		t.Fatalf("expected 1 detected file URI, got %d", len(files))
	}
	if files[0].Raw != "file://out/app.tar.gz" {
		t.Fatalf("unexpected raw URI: %q", files[0].Raw)
	}
}

func TestStripDetectedFileURIs_RemovesStandaloneURIs(t *testing.T) {
	text := "Created output file://reports/summary.txt and file:///tmp/build.log"
	files := detectFileURIs(text)
	got := stripDetectedFileURIs(text, files)
	if strings.Contains(got, "file://") {
		t.Fatalf("expected no file URI in output, got %q", got)
	}
}

func TestStripDetectedFileURIs_RewritesMarkdownLinks(t *testing.T) {
	text := "Use [artifact](file://out/app.tar.gz)."
	files := detectFileURIs(text)
	got := stripDetectedFileURIs(text, files)
	want := "Use artifact."
	if got != want {
		t.Fatalf("stripDetectedFileURIs() = %q, want %q", got, want)
	}
}

func TestResolveFilePath_RepoRelative(t *testing.T) {
	repo := t.TempDir()
	path, err := resolveFilePath(repo, "build/output.txt")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !strings.HasPrefix(path, repo+string(os.PathSeparator)) {
		t.Fatalf("expected resolved path inside repo, got %q", path)
	}
}

func TestResolveFilePath_RejectsOutsideRepo(t *testing.T) {
	repo := t.TempDir()
	if _, err := resolveFilePath(repo, "../secrets.txt"); err == nil {
		t.Fatalf("expected outside-repo path to be rejected")
	}
}

func TestResolveFirstDetectedFilePath_PicksFirstSendable(t *testing.T) {
	repo := t.TempDir()
	validPath := filepath.Join(repo, "out.txt")
	if err := os.WriteFile(validPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	files := []detectedFileURI{
		{Raw: "file://missing.txt", Path: "missing.txt"},
		{Raw: "file://out.txt", Path: "out.txt"},
	}

	resolved, idx, err := resolveFirstDetectedFilePath(repo, files)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected idx 1, got %d", idx)
	}
	if resolved != validPath {
		t.Fatalf("resolved path = %q, want %q", resolved, validPath)
	}
}

func TestResolveFirstDetectedFilePath_NoSendableFiles(t *testing.T) {
	repo := t.TempDir()
	files := []detectedFileURI{
		{Raw: "file://missing.txt", Path: "missing.txt"},
		{Raw: "file://../outside.txt", Path: "../outside.txt"},
	}

	_, _, err := resolveFirstDetectedFilePath(repo, files)
	if err == nil {
		t.Fatalf("expected error for no sendable files")
	}
}

func TestRemoveDetectedFileByIndex(t *testing.T) {
	files := []detectedFileURI{
		{Raw: "file://one.txt", Path: "one.txt"},
		{Raw: "file://two.txt", Path: "two.txt"},
		{Raw: "file://three.txt", Path: "three.txt"},
	}
	got := removeDetectedFileByIndex(files, 1)
	if len(got) != 2 {
		t.Fatalf("expected 2 files, got %d", len(got))
	}
	if got[0].Raw != "file://one.txt" || got[1].Raw != "file://three.txt" {
		t.Fatalf("unexpected files after removal: %+v", got)
	}
}

func TestFormatAgentFailureResponse_ErrorOnly(t *testing.T) {
	got := formatAgentFailureResponse(errors.New("exit status 1"), "")
	want := "Agent failed to run.\nError: exit status 1"
	if got != want {
		t.Fatalf("formatAgentFailureResponse() = %q, want %q", got, want)
	}
}

func TestFormatAgentFailureResponse_WithAgentOutput(t *testing.T) {
	got := formatAgentFailureResponse(errors.New("exit status 1"), "failed to find CODEX_BIN")
	want := "Agent failed to run.\nError: exit status 1\n\nAgent output:\nfailed to find CODEX_BIN"
	if got != want {
		t.Fatalf("formatAgentFailureResponse() = %q, want %q", got, want)
	}
}

func TestFormatAgentFailureResponse_DeduplicatesErrorText(t *testing.T) {
	got := formatAgentFailureResponse(errors.New("exit status 1"), "exit status 1")
	want := "Agent failed to run.\nError: exit status 1"
	if got != want {
		t.Fatalf("formatAgentFailureResponse() = %q, want %q", got, want)
	}
}

func TestSanitizeAgentCommitMessage_SimpleLine(t *testing.T) {
	got := sanitizeAgentCommitMessage("Add branch-aware commit flow")
	want := "Add branch-aware commit flow"
	if got != want {
		t.Fatalf("sanitizeAgentCommitMessage() = %q, want %q", got, want)
	}
}

func TestSanitizeAgentCommitMessage_DropsSessionMarker(t *testing.T) {
	got := sanitizeAgentCommitMessage("New session started.\nAdd commit message generation")
	want := "Add commit message generation"
	if got != want {
		t.Fatalf("sanitizeAgentCommitMessage() = %q, want %q", got, want)
	}
}

func TestSanitizeAgentCommitMessage_StripsPrefixAndQuotes(t *testing.T) {
	got := sanitizeAgentCommitMessage("Commit message: \"Add commit message generation\"")
	want := "Add commit message generation"
	if got != want {
		t.Fatalf("sanitizeAgentCommitMessage() = %q, want %q", got, want)
	}
}

func TestSanitizeAgentCommitMessage_Empty(t *testing.T) {
	got := sanitizeAgentCommitMessage(" \n\t")
	if got != "" {
		t.Fatalf("sanitizeAgentCommitMessage() = %q, want empty", got)
	}
}

func TestSanitizeAgentPRBody_PreservesBullets(t *testing.T) {
	got := sanitizeAgentPRBody("- Add commit message generation\n- Improve PR flow")
	want := "- Add commit message generation\n- Improve PR flow"
	if got != want {
		t.Fatalf("sanitizeAgentPRBody() = %q, want %q", got, want)
	}
}

func TestSanitizeAgentPRBody_DropsSessionMarkerAndPrefix(t *testing.T) {
	got := sanitizeAgentPRBody("New session started.\nPR description: - Add commit flow updates")
	want := "- Add commit flow updates"
	if got != want {
		t.Fatalf("sanitizeAgentPRBody() = %q, want %q", got, want)
	}
}

func TestSanitizeAgentPRBody_StripsCodeFences(t *testing.T) {
	got := sanitizeAgentPRBody("```markdown\n- Add commit flow updates\n- Document fallback\n```")
	want := "- Add commit flow updates\n- Document fallback"
	if got != want {
		t.Fatalf("sanitizeAgentPRBody() = %q, want %q", got, want)
	}
}

func TestSanitizeAgentPRBody_Empty(t *testing.T) {
	got := sanitizeAgentPRBody(" \n\t")
	if got != "" {
		t.Fatalf("sanitizeAgentPRBody() = %q, want empty", got)
	}
}
