package main

import (
	"strings"
	"testing"
)

func TestParseFeedStreamRSS(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel>
    <title>测试播客</title>
    <description>这是一个用于流式解析测试的播客</description>
    <itunes:image href="http://example.com/cover.jpg"/>
    <item>
      <title>第1集：开篇</title>
      <enclosure url="http://example.com/ep1.mp3" length="123456" type="audio/mpeg"/>
      <pubDate>Wed, 28 Oct 2026 12:00:00 +0800</pubDate>
    </item>
    <item>
      <title>第2集：进阶</title>
      <enclosure url="http://example.com/ep2.mp3" length="234567" type="audio/mpeg"/>
      <pubDate>Thu, 29 Oct 2026 12:00:00 +0800</pubDate>
    </item>
    <item>
      <title>第3集：终章</title>
      <enclosure url="http://example.com/ep3.mp3" length="345678" type="audio/mpeg"/>
      <pubDate>Fri, 30 Oct 2026 12:00:00 +0800</pubDate>
    </item>
  </channel>
</rss>`

	feed, err := parseFeedStream(strings.NewReader(xmlData), "http://example.com/feed.xml", 2)
	if err != nil {
		t.Fatalf("parseFeedStream failed: %v", err)
	}

	if feed.Title != "测试播客" {
		t.Errorf("feed.Title = %q, want %q", feed.Title, "测试播客")
	}
	if feed.Cover != "http://example.com/cover.jpg" {
		t.Errorf("feed.Cover = %q, want %q", feed.Cover, "http://example.com/cover.jpg")
	}
	// Max items was 2, so should only parse 2 items even though 3 exist
	if len(feed.Episodes) != 2 {
		t.Errorf("len(feed.Episodes) = %d, want 2 (early stop)", len(feed.Episodes))
	}
	if feed.Episodes[0].Title != "第1集：开篇" {
		t.Errorf("Episodes[0].Title = %q, want %q", feed.Episodes[0].Title, "第1集：开篇")
	}
	if feed.Episodes[0].AudioURL != "http://example.com/ep1.mp3" {
		t.Errorf("Episodes[0].AudioURL = %q, want %q", feed.Episodes[0].AudioURL, "http://example.com/ep1.mp3")
	}
}

func TestParseFeedStreamAtom(t *testing.T) {
	atomData := `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Atom测试播客</title>
  <logo>http://example.com/atom_logo.png</logo>
  <entry>
    <title>Atom第1期</title>
    <id>atom-1</id>
    <link rel="enclosure" type="audio/mpeg" href="http://example.com/atom1.mp3"/>
    <published>2026-10-28T10:00:00Z</published>
  </entry>
</feed>`

	feed, err := parseFeedStream(strings.NewReader(atomData), "http://example.com/atom.xml", 10)
	if err != nil {
		t.Fatalf("parseFeedStream failed for Atom: %v", err)
	}
	if feed.Title != "Atom测试播客" {
		t.Errorf("feed.Title = %q, want %q", feed.Title, "Atom测试播客")
	}
	if len(feed.Episodes) != 1 {
		t.Fatalf("len(feed.Episodes) = %d, want 1", len(feed.Episodes))
	}
	if feed.Episodes[0].AudioURL != "http://example.com/atom1.mp3" {
		t.Errorf("Episodes[0].AudioURL = %q, want %q", feed.Episodes[0].AudioURL, "http://example.com/atom1.mp3")
	}
}

func TestFeedEpisodesEqual(t *testing.T) {
	ep1 := []PodcastItem{
		{Title: "第1集", AudioURL: "http://example.com/1.mp3", Duration: "10:00", Downloaded: false},
		{Title: "第2集", AudioURL: "http://example.com/2.mp3", Duration: "20:00", Downloaded: false},
	}
	ep2 := []PodcastItem{
		{Title: "第1集", AudioURL: "http://example.com/1.mp3", Duration: "10:00", Downloaded: false},
		{Title: "第2集", AudioURL: "http://example.com/2.mp3", Duration: "20:00", Downloaded: false},
	}
	if !feedEpisodesEqual(ep1, ep2) {
		t.Errorf("feedEpisodesEqual should be true for identical lists")
	}

	// Different length
	ep3 := append([]PodcastItem(nil), ep1...)
	ep3 = append(ep3, PodcastItem{Title: "第3集", AudioURL: "http://example.com/3.mp3"})
	if feedEpisodesEqual(ep1, ep3) {
		t.Errorf("feedEpisodesEqual should be false for different lengths")
	}

	// Different title
	ep4 := []PodcastItem{
		{Title: "第1集（修订）", AudioURL: "http://example.com/1.mp3", Duration: "10:00", Downloaded: false},
		{Title: "第2集", AudioURL: "http://example.com/2.mp3", Duration: "20:00", Downloaded: false},
	}
	if feedEpisodesEqual(ep1, ep4) {
		t.Errorf("feedEpisodesEqual should be false for modified episode title")
	}

	// Different URL
	ep5 := []PodcastItem{
		{Title: "第1集", AudioURL: "http://example.com/1-new.mp3", Duration: "10:00", Downloaded: false},
		{Title: "第2集", AudioURL: "http://example.com/2.mp3", Duration: "20:00", Downloaded: false},
	}
	if feedEpisodesEqual(ep1, ep5) {
		t.Errorf("feedEpisodesEqual should be false for modified audio URL")
	}
}
