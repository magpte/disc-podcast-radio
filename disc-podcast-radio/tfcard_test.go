package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecideTFImport(t *testing.T) {
	cases := []struct {
		name        string
		firstRun    bool
		lastHash    string
		fileHash    string
		tfFileNewer bool
		want        tfImportMode
	}{
		{"fresh install trusts tf card", true, "", "h1", false, tfImportAuthoritative},
		{"upgrade with newer tf edit", false, "", "h1", true, tfImportAuthoritative},
		{"upgrade with stale tf file keeps local list", false, "", "h1", false, tfImportSkip},
		{"unchanged own mirror is skipped", false, "h1", "h1", true, tfImportSkip},
		{"external edit is imported", false, "h1", "h2", false, tfImportAuthoritative},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideTFImport(tc.firstRun, tc.lastHash, tc.fileHash, tc.tfFileNewer)
			if got != tc.want {
				t.Fatalf("decideTFImport() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTFFileNewerThan(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "podcasts.txt")
	data := filepath.Join(dir, "podcasts.json")
	if err := os.WriteFile(tf, []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, []byte("b"), 0644); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-time.Hour)
	now := time.Now()
	if err := os.Chtimes(data, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tf, old, old); err != nil {
		t.Fatal(err)
	}
	if tfFileNewerThan(tf, data) {
		t.Fatal("stale TF file must not be considered newer than local data")
	}

	if err := os.Chtimes(tf, now.Add(time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !tfFileNewerThan(tf, data) {
		t.Fatal("edited TF file must be considered newer")
	}

	if !tfFileNewerThan(tf, filepath.Join(dir, "missing.json")) {
		t.Fatal("missing reference must count as older")
	}
}

func TestTFCommentHeaderPreservesUserComments(t *testing.T) {
	existing := "# user note\r\nname,http://example.com/feed\r\n# second note\r\n"
	got := tfCommentHeader(existing, "fallback\n")

	if !strings.Contains(got, "# user note\n") || !strings.Contains(got, "# second note\n") {
		t.Fatalf("user comments were not preserved: %q", got)
	}
	if strings.Contains(got, "http://example.com/feed") {
		t.Fatalf("entry line leaked into comment header: %q", got)
	}
	if !strings.Contains(got, "# second note\n\n") {
		t.Fatalf("comment header must be separated from entries by a blank line: %q", got)
	}

	fallback := tfCommentHeader("", "fallback\n")
	if fallback != "fallback\n\n" {
		t.Fatalf("fallback header = %q", fallback)
	}
}

func TestExportUIMenusPersistsSnapshots(t *testing.T) {
	dir := t.TempDir()
	a := &App{dataDir: dir}
	a.podcasts = []PodcastFeed{{
		Title: "测试播客",
		Episodes: []PodcastItem{{
			Title:    "第 1 集",
			AudioURL: "https://example.com/ep1.mp3",
		}},
	}}
	a.stations = []RadioStation{{
		Name:     "测试电台",
		URL:      "https://example.com/radio.mp3",
		Category: "测试分类",
	}}

	a.exportEpisodesText()
	a.exportRadiosText()

	ep, err := os.ReadFile(filepath.Join(dir, "episodes.txt"))
	if err != nil {
		t.Fatalf("episodes snapshot missing: %v", err)
	}
	if !strings.Contains(string(ep), "[测试播客]") || !strings.Contains(string(ep), "第 1 集 | https://example.com/ep1.mp3") {
		t.Fatalf("episodes snapshot content wrong: %q", ep)
	}

	rd, err := os.ReadFile(filepath.Join(dir, "radios.txt"))
	if err != nil {
		t.Fatalf("radios snapshot missing: %v", err)
	}
	if !strings.Contains(string(rd), "[测试分类]") || !strings.Contains(string(rd), "测试电台 | https://example.com/radio.mp3") {
		t.Fatalf("radios snapshot content wrong: %q", rd)
	}
}
