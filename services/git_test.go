package services

import "testing"

func TestExtractGitHubURL_ExactURL(t *testing.T) {
	got := extractGitHubURL("https://github.com/acme/repo/pull/42")
	want := "https://github.com/acme/repo/pull/42"
	if got != want {
		t.Fatalf("extractGitHubURL() = %q, want %q", got, want)
	}
}

func TestExtractGitHubURL_EmbeddedInErrorOutput(t *testing.T) {
	got := extractGitHubURL("a pull request already exists for branch\nhttps://github.com/acme/repo/pull/42")
	want := "https://github.com/acme/repo/pull/42"
	if got != want {
		t.Fatalf("extractGitHubURL() = %q, want %q", got, want)
	}
}

func TestExtractGitHubURL_StripsTrailingPunctuation(t *testing.T) {
	got := extractGitHubURL("existing PR: https://github.com/acme/repo/pull/42)")
	want := "https://github.com/acme/repo/pull/42"
	if got != want {
		t.Fatalf("extractGitHubURL() = %q, want %q", got, want)
	}
}

func TestExtractGitHubURL_NoMatch(t *testing.T) {
	got := extractGitHubURL("no URL here")
	if got != "" {
		t.Fatalf("extractGitHubURL() = %q, want empty", got)
	}
}
