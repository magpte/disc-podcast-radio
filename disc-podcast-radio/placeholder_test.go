package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlaceholderGeneration(t *testing.T) {
	root := t.TempDir()
	a := &App{playlistRoot: root, media: map[string]*MediaEntry{}}
	entry := &MediaEntry{
		Token:    "0123456789abcdef",
		Kind:     "radio",
		ID:       "radio_test",
		URL:      "http://example.com/live.m3u8",
		Title:    "中国之声",
		Subtitle: "国家台",
	}
	a.media[entry.Token] = entry
	p, err := a.ensurePlaceholder(entry, "radio", "国家台", "中国之声")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "中国之声.mp3" {
		t.Fatalf("unexpected filename: %s", p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("ID3")) {
		t.Fatal("placeholder has no ID3 tag")
	}
	if !bytes.Contains(data, id3UTF16LE("中国之声")) {
		t.Fatal("placeholder title missing from ID3")
	}
	if len(data) <= len(silenceMP3) {
		t.Fatal("placeholder has no silent MP3 payload")
	}
	// Rebuilding must reuse the same path and not create "(2)".
	again, err := a.ensurePlaceholder(entry, "radio", "国家台", "中国之声")
	if err != nil {
		t.Fatal(err)
	}
	if again != p {
		t.Fatalf("placeholder path changed: %s -> %s", p, again)
	}
}

func TestPlaceholderIsValidMP3(t *testing.T) {
	ffprobe := os.Getenv("FFPROBE")
	if ffprobe == "" {
		ffprobe = `C:\Users\huoweimin\tool\ffmpeg\ffprobe.exe`
	}
	if _, err := os.Stat(ffprobe); err != nil {
		t.Skip("ffprobe not available")
	}
	root := t.TempDir()
	a := &App{playlistRoot: root, media: map[string]*MediaEntry{}}
	entry := &MediaEntry{Token: "0123456789abcdef", Kind: "radio", URL: "http://example.com/live", Title: "中国之声"}
	a.media[entry.Token] = entry
	p, err := a.ensurePlaceholder(entry, "radio", "国家台", "中国之声")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=format_name,duration:format_tags=title,artist,album", "-of", "json", p).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe rejected placeholder: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{`"format_name": "mp3"`, `"title": "中国之声"`, `"album": "DISC 网络电台"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("ffprobe output missing %s:\n%s", want, text)
		}
	}
}

func TestParseRendererDescription(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>
    <friendlyName>mq_player</friendlyName>
    <modelName>DISC</modelName>
    <UDN>uuid:test</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:AVTransport</serviceId>
        <SCPDURL>/upnp/rendertransportSCPD.xml</SCPDURL>
        <controlURL>/upnp/control/rendertransport1</controlURL>
        <eventSubURL>/upnp/event/rendertransport1</eventSubURL>
      </service>
      <service>
        <serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:RenderingControl</serviceId>
        <controlURL>/upnp/control/rendercontrol1</controlURL>
        <eventSubURL>/upnp/event/rendercontrol1</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>`)
	r, err := parseRendererDescription(body, "http://127.0.0.1:1286/description.xml", "test")
	if err != nil {
		t.Fatal(err)
	}
	if r.FriendlyName != "mq_player" {
		t.Fatalf("friendly name = %q", r.FriendlyName)
	}
	want := "http://127.0.0.1:1286/upnp/control/rendertransport1"
	if r.ControlURL != want {
		t.Fatalf("control URL = %q, want %q", r.ControlURL, want)
	}
	if r.RenderingControlURL == "" {
		t.Fatal("RenderingControl URL missing")
	}
}

func TestParseRendererDescriptionKeepsReachableHost(t *testing.T) {
	body := []byte(`<root><device><friendlyName>mq_player</friendlyName><serviceList><service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType><controlURL>/upnp/control/rendertransport1</controlURL></service></serviceList></device></root>`)
	r, err := parseRendererDescription(body, "http://10.20.30.40:1286/description.xml", "test")
	if err != nil {
		t.Fatal(err)
	}
	want := "http://10.20.30.40:1286/upnp/control/rendertransport1"
	if r.ControlURL != want {
		t.Fatalf("control URL = %q, want %q", r.ControlURL, want)
	}
	if got := localizeURL("http://10.20.30.40:1286/"); got != "http://10.20.30.40:1286/" {
		t.Fatalf("localizeURL changed reachable LAN URL: %q", got)
	}
	if got := localizeURL("http://0.0.0.0:1286/description.xml"); got != "http://127.0.0.1:1286/description.xml" {
		t.Fatalf("localizeURL unspecified = %q", got)
	}
	if got := replaceUnspecifiedHost("http://0.0.0.0:1286/description.xml", net.IPv4(10, 0, 0, 5)); got != "http://10.0.0.5:1286/description.xml" {
		t.Fatalf("replaceUnspecifiedHost = %q", got)
	}
	if !isLocalURLHost("http://127.0.0.1:1286/") || !isLocalURLHost("http://0.0.0.0:1286/") {
		t.Fatal("loopback/unspecified URL should count as local")
	}
}

func TestProbeRendererCandidatesFindsDescription(t *testing.T) {
	description := `<root><device><friendlyName>mock</friendlyName><serviceList><service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType><controlURL>/control</controlURL></service></serviceList></device></root>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/description.xml" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(description))
	}))
	defer srv.Close()

	a := &App{soapClient: &http.Client{Timeout: 2 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, attempts := a.probeRendererCandidates(ctx, []rendererCandidate{{
		URL: srv.URL, Source: "test", Timeout: time.Second,
	}}, false)
	if r == nil {
		t.Fatalf("renderer not found: %+v", attempts)
	}
	if r.FriendlyName != "mock" || r.ControlURL != srv.URL+"/control" {
		t.Fatalf("unexpected renderer: %+v", r)
	}
}

func TestSanitizeFilenameControlChars(t *testing.T) {
	got := sanitizeFilename("a\r\nb\tc")
	if strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("control characters survived: %q", got)
	}
	if got == "" {
		t.Fatal("sanitized name is empty")
	}
}

func TestNormalizePlayerPath(t *testing.T) {
	cases := map[string]string{
		"file:///tmp/sdcard/a%20b.mp3":  "/tmp/sdcard/a b.mp3",
		"/tmp/sdcard/a.mp3 (deleted)":   "/tmp/sdcard/a.mp3",
		"http://127.0.0.1:8888/audio/x": "",
		"":                              "",
	}
	for in, want := range cases {
		if got := normalizePlayerPath(in); got != filepath.Clean(want) && !(want == "" && got == "") {
			t.Errorf("normalizePlayerPath(%q)=%q, want %q", in, got, want)
		}
	}
}
