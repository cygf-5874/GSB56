// Package logfmt 把结构化字段编码成 logfmt 文本（`k=v` 以单个空格分隔）。
//
// 字段顺序、转义表与值类型的格式见 README.md 的「对外契约」一节。
package logfmt

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Field 是一个待编码的字段。
//
// Value 支持的静态类型见 README 的格式表；其余类型等价于 fmt.Sprint(v)。
type Field struct {
	Key   string
	Value any
}

// Validate 检查 fields 里有没有空 key。
//
// nil 或空切片返回 nil；任意一个字段的 Key 是 "" 时返回 ErrEmptyKey。
func Validate(fields []Field) error {
	for i := range fields {
		if fields[i].Key == "" {
			return ErrEmptyKey
		}
	}
	return nil
}

// Encode 把 fields 编码后追加到 dst 末尾并返回。
//
// 字段之间用单个空格分隔；dst 非空时第一个字段之前也补一个空格。
func Encode(dst []byte, fields []Field) []byte {
	if len(fields) == 0 {
		return dst
	}

	// 去重：每个 key 保留最后一次出现的值，位置取首次出现的位置。
	values := make(map[string]string, len(fields))
	firstAt := make(map[string]int, len(fields))
	keys := make([]string, 0, len(fields))
	for i := range fields {
		k := fields[i].Key
		if _, seen := firstAt[k]; !seen {
			firstAt[k] = i
			keys = append(keys, k)
		}
		values[k] = formatValue(fields[i].Value)
	}
	sort.Slice(keys, func(i, j int) bool { return firstAt[keys[i]] < firstAt[keys[j]] })

	var b strings.Builder
	nonEmpty := len(dst) > 0
	for i := range keys {
		if i > 0 || nonEmpty {
			b.WriteByte(' ')
		}
		b.WriteString(keys[i])
		b.WriteByte('=')
		escapeInto(&b, values[keys[i]])
	}

	return append(dst, b.String()...)
}

// String 把 fields 编码成字符串。
func String(fields []Field) string {
	return string(Encode(nil, fields))
}

// formatValue 把单个值渲染成未转义的字符串。
func formatValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case time.Duration:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}

// escapeInto 按转义表把 s 写进 b。
func escapeInto(b *strings.Builder, s string) {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '\\':
				b.WriteString(`\\`)
			case '"':
				b.WriteString(`\"`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			case ' ', '=':
				writeHex(b, c)
			default:
				if c < 0x20 {
					writeHex(b, c)
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			writeHex(b, c)
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
}

func writeHex(b *strings.Builder, c byte) {
	const digits = "0123456789abcdef"
	b.WriteString(`\x`)
	b.WriteByte(digits[c>>4])
	b.WriteByte(digits[c&0x0f])
}
