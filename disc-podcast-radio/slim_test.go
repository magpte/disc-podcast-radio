package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestSlimReconnectDelay(t *testing.T) {
	base := time.Second
	max := 60 * time.Second
	cases := []struct {
		doublings int
		want      time.Duration
	}{
		{-1, time.Second},
		{0, time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second},
		{20, 60 * time.Second},
	}
	for _, c := range cases {
		if got := reconnectDelay(c.doublings, base, max); got != c.want {
			t.Errorf("reconnectDelay(%d) = %v, want %v", c.doublings, got, c.want)
		}
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestSlimCharsetReader(t *testing.T) {
	// UTF-8 (and the empty name) must pass through untouched.
	for _, name := range []string{"", "utf-8", "UTF8", " us-ascii "} {
		in := "中文 abc"
		if got := readAll(t, mustReader(t, name, in)); got != in {
			t.Errorf("%q passthrough = %q, want %q", name, got, in)
		}
	}

	// GBK bytes for 中文.
	gbk := string([]byte{0xD6, 0xD0, 0xCE, 0xC4})
	for _, name := range []string{"gbk", "GB2312", "gb18030", "x-gbk", "cp936"} {
		if got := readAll(t, mustReader(t, name, gbk)); got != "中文" {
			t.Errorf("%q decode = %q, want 中文", name, got)
		}
	}

	// Big5 bytes for 中文.
	big5 := string([]byte{0xA4, 0xA4, 0xA4, 0xE5})
	for _, name := range []string{"big5", "Big-5", "cp950"} {
		if got := readAll(t, mustReader(t, name, big5)); got != "中文" {
			t.Errorf("%q decode = %q, want 中文", name, got)
		}
	}

	// An unknown charset (e.g. Shift_JIS) must degrade to passthrough, which
	// matches the old htmlindex error path.
	raw := string([]byte{0x82, 0xA0})
	if got := readAll(t, mustReader(t, "shift_jis", raw)); got != raw {
		t.Errorf("unknown charset passthrough = %q, want %q", got, raw)
	}
}

func mustReader(t *testing.T, charset, body string) io.Reader {
	t.Helper()
	r, err := prCharsetReader(charset, strings.NewReader(body))
	if err != nil {
		t.Fatalf("prCharsetReader(%q): %v", charset, err)
	}
	return r
}
