package main

import (
	"io"
	"strings"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"
)

// prCharsetReader decodes the character sets that podcast feeds actually use.
//
// The previous implementation called htmlindex.Get, which links in every
// legacy encoding in the WHATWG index (Japanese, Korean, all single-byte
// charmaps and the full GB/Big5 tables). That accounted for roughly 590 KB of
// the stripped binary. Field feeds are UTF-8 or one of the Chinese legacy
// encodings below; anything else is passed through unchanged, which is the
// same fallback the old code used when htmlindex.Get returned an error.
func prCharsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii", "unicode-1-1-utf-8":
		return input, nil
	case "gbk", "gb2312", "gb18030", "x-gbk", "cp936", "x-gb18030":
		return transform.NewReader(input, simplifiedchinese.GB18030.NewDecoder()), nil
	case "big5", "big-5", "cp950", "big5-hkscs":
		return transform.NewReader(input, traditionalchinese.Big5.NewDecoder()), nil
	case "iso-8859-1", "latin1", "windows-1252", "cp1252":
		return transform.NewReader(input, charmap.Windows1252.NewDecoder()), nil
	}
	return input, nil
}
