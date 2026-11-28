package core

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

type jsonTextState uint8

const (
	jsSeekObject jsonTextState = iota
	jsSeekKey
	jsReadKey
	jsSeekColon
	jsSeekVal
	jsCaptureStr
	jsSkipStr
	jsSkipPrim
	jsDone
)

// replyUserTextStreamExtractor incrementally extracts the top-level JSON string field "text"
// from streaming reply_user tool call arguments across arbitrary chunk boundaries, handling
// JSON escapes (\n, \t, \", \\) and UTF-16 \uXXXX (including surrogate pairs) deterministically.
type replyUserTextStreamExtractor struct {
	state         jsonTextState
	depth         int
	skipDepth     int
	inEscape      bool
	inUnicode     bool
	uHex          [4]byte
	uLen          int
	highSurrogate rune
	keyBuf        strings.Builder
	textBuf       strings.Builder
	wantCapture   bool
	failed        bool
}

func (p *replyUserTextStreamExtractor) Feed(chunk string) (text string, changed bool) {
	if p.failed || p.state == jsDone || chunk == "" {
		return p.textBuf.String(), false
	}
	beforeLen := p.textBuf.Len()
	for i := 0; i < len(chunk); i++ {
		if p.failed || p.state == jsDone {
			break
		}
		p.stepByte(chunk[i])
	}
	return p.textBuf.String(), p.textBuf.Len() != beforeLen
}

func (p *replyUserTextStreamExtractor) Text() string {
	return p.textBuf.String()
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func (p *replyUserTextStreamExtractor) stepByte(b byte) {
	switch p.state {
	case jsSeekObject:
		if isJSONSpace(b) {
			return
		}
		if b == '{' {
			p.depth = 1
			p.state = jsSeekKey
			return
		}
		p.failed = true

	case jsSeekKey:
		if isJSONSpace(b) || b == ',' {
			return
		}
		if b == '}' {
			p.state = jsDone
			return
		}
		if b == '"' {
			p.keyBuf.Reset()
			p.inEscape, p.inUnicode, p.uLen, p.highSurrogate = false, false, 0, 0
			p.state = jsReadKey
			return
		}
		p.failed = true

	case jsReadKey:
		if p.feedStringByte(b, &p.keyBuf) {
			p.wantCapture = p.keyBuf.String() == "text"
			p.state = jsSeekColon
		}

	case jsSeekColon:
		if isJSONSpace(b) {
			return
		}
		if b == ':' {
			p.state = jsSeekVal
			return
		}
		p.failed = true

	case jsSeekVal:
		if isJSONSpace(b) {
			return
		}
		switch b {
		case '"':
			p.inEscape, p.inUnicode, p.uLen, p.highSurrogate = false, false, 0, 0
			if p.wantCapture {
				p.state = jsCaptureStr
			} else {
				p.skipDepth = 0
				p.state = jsSkipStr
			}
		case '{', '[':
			p.wantCapture = false
			p.skipDepth = 1
			p.state = jsSkipPrim
		default:
			p.wantCapture = false
			p.skipDepth = 0
			p.state = jsSkipPrim
		}

	case jsCaptureStr:
		if p.feedStringByte(b, &p.textBuf) {
			p.state = jsDone
		}

	case jsSkipStr:
		if p.inEscape {
			p.inEscape = false
			return
		}
		if b == '\\' {
			p.inEscape = true
			return
		}
		if b == '"' {
			if p.skipDepth > 0 {
				p.state = jsSkipPrim
			} else {
				p.state = jsSeekKey
			}
		}

	case jsSkipPrim:
		if p.skipDepth > 0 {
			switch b {
			case '"':
				p.inEscape = false
				p.state = jsSkipStr
			case '{', '[':
				p.skipDepth++
			case '}', ']':
				p.skipDepth--
				if p.skipDepth == 0 {
					p.state = jsSeekKey
				}
			}
			return
		}
		switch b {
		case ',':
			p.state = jsSeekKey
		case '}':
			p.state = jsDone
		}
	}
}

func (p *replyUserTextStreamExtractor) feedStringByte(b byte, dst *strings.Builder) bool {
	if p.inUnicode {
		p.uHex[p.uLen] = b
		p.uLen++
		if p.uLen < 4 {
			return false
		}
		p.inUnicode, p.uLen = false, 0
		v, err := strconv.ParseUint(string(p.uHex[:]), 16, 16)
		if err != nil {
			p.failed = true
			return false
		}
		r := rune(v)
		if p.highSurrogate != 0 {
			if utf16.IsSurrogate(r) && r >= 0xDC00 && r <= 0xDFFF {
				dst.WriteRune(utf16.DecodeRune(p.highSurrogate, r))
				p.highSurrogate = 0
				return false
			}
			dst.WriteRune(p.highSurrogate)
			p.highSurrogate = 0
		}
		if r >= 0xD800 && r <= 0xDBFF {
			p.highSurrogate = r
			return false
		}
		dst.WriteRune(r)
		return false
	}
	if p.inEscape {
		p.inEscape = false
		if p.highSurrogate != 0 && b != 'u' {
			dst.WriteRune(p.highSurrogate)
			p.highSurrogate = 0
		}
		switch b {
		case '"', '\\', '/':
			dst.WriteByte(b)
		case 'b':
			dst.WriteByte('\b')
		case 'f':
			dst.WriteByte('\f')
		case 'n':
			dst.WriteByte('\n')
		case 'r':
			dst.WriteByte('\r')
		case 't':
			dst.WriteByte('\t')
		case 'u':
			p.inUnicode, p.uLen = true, 0
		default:
			dst.WriteByte(b)
		}
		return false
	}
	if b == '\\' {
		p.inEscape = true
		return false
	}
	if p.highSurrogate != 0 {
		dst.WriteRune(p.highSurrogate)
		p.highSurrogate = 0
	}
	if b == '"' {
		return true
	}
	dst.WriteByte(b)
	return false
}
