package logfmt_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"logfmt"
)

// ------------------------------------------------------------------ 用例

func TestEncodeBasicPairs(t *testing.T) {
	got := logfmt.Encode(nil, []logfmt.Field{
		{Key: "a", Value: int64(1)},
		{Key: "b", Value: "x"},
	})
	if want := "a=1 b=x"; string(got) != want {
		t.Fatalf("Encode = %q, want %q", got, want)
	}

	// 值里的空白按转义表处理，不会破坏 `k=v` 的边界。
	got = logfmt.Encode(nil, []logfmt.Field{{Key: "msg", Value: "hello world"}})
	if want := `msg=hello\x20world`; string(got) != want {
		t.Fatalf("Encode = %q, want %q", got, want)
	}
}

func TestEncodeNilAndEmptyFields(t *testing.T) {
	if got := logfmt.Encode(nil, nil); len(got) != 0 {
		t.Fatalf("Encode(nil, nil) = %q, 长度应为 0", got)
	}
	if got := logfmt.Encode(nil, []logfmt.Field{}); len(got) != 0 {
		t.Fatalf("Encode(nil, []) = %q, 长度应为 0", got)
	}
	if got := logfmt.String(nil); got != "" {
		t.Fatalf("String(nil) = %q, want \"\"", got)
	}
	if got := logfmt.Encode([]byte("KEEP"), nil); string(got) != "KEEP" {
		t.Fatalf("Encode 空字段时改动了 dst: %q", got)
	}
}

func TestFieldOrderIsFirstAppearance(t *testing.T) {
	got := logfmt.Encode(nil, []logfmt.Field{
		{Key: "zeta", Value: int64(1)},
		{Key: "alpha", Value: int64(2)},
		{Key: "mid", Value: int64(3)},
	})
	if want := "zeta=1 alpha=2 mid=3"; string(got) != want {
		t.Fatalf("Encode = %q, want %q（必须按首次出现顺序，不是字典序）", got, want)
	}
}

func TestDuplicateKeyKeepsLastValueAndFirstPosition(t *testing.T) {
	got := logfmt.Encode(nil, []logfmt.Field{
		{Key: "b", Value: int64(1)},
		{Key: "a", Value: int64(2)},
		{Key: "b", Value: int64(3)},
		{Key: "a", Value: int64(4)},
		{Key: "c", Value: int64(5)},
	})
	if want := "b=3 a=4 c=5"; string(got) != want {
		t.Fatalf("Encode = %q, want %q（重复 key 取最后一次的值、首次出现的位置）", got, want)
	}
}

func TestValueEscapeTable(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"反斜杠", `a\b`, `a\\b`},
		{"双引号", `a"b`, `a\"b`},
		{"换行", "a\nb", `a\nb`},
		{"回车", "a\rb", `a\rb`},
		{"制表", "a\tb", `a\tb`},
		{"空格", "a b", `a\x20b`},
		{"等号", "a=b", `a\x3db`},
		{"控制字节", "a\x01\x1fb", `a\x01\x1fb`},
		{"原样保留", "aZ9_.-b", "aZ9_.-b"},
		{"全表", "a b=c\"d\\e\n\r\t\x01\x1f", `a\x20b\x3dc\"d\\e\n\r\t\x01\x1f`},
	}
	for _, tc := range cases {
		got := logfmt.Encode(nil, []logfmt.Field{{Key: "k", Value: tc.value}})
		if want := "k=" + tc.want; string(got) != want {
			t.Fatalf("%s: Encode = %q, want %q", tc.name, got, want)
		}
	}
}

func TestInvalidUTF8EscapedPerByte(t *testing.T) {
	got := logfmt.Encode(nil, []logfmt.Field{{Key: "k", Value: []byte{0xff, 'a', 0xfe, 0x80}}})
	if want := `k=\xffa\xfe\x80`; string(got) != want {
		t.Fatalf("Encode = %q, want %q", got, want)
	}

	// 合法的多字节 UTF-8 原样保留，替换字符（U+FFFD，EF BF BD）也一样。
	got = logfmt.Encode(nil, []logfmt.Field{{Key: "k", Value: "日本語\ufffd"}})
	if want := "k=日本語\ufffd"; string(got) != want {
		t.Fatalf("Encode = %q, want %q", got, want)
	}

	// 合法序列后面紧跟一个孤立字节：只有孤立字节被逐字节转义。
	got = logfmt.Encode(nil, []logfmt.Field{{Key: "k", Value: []byte("日\xc3")}})
	if want := "k=日\\xc3"; string(got) != want {
		t.Fatalf("Encode = %q, want %q", got, want)
	}
}

func TestValueTypeFormats(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"string", "s", "s"},
		{"bytes", []byte("xy"), "xy"},
		{"int64", int64(-8443), "-8443"},
		{"float64", 0.125, "0.125"},
		{"float64 整数", 3.0, "3"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"duration 毫秒", 1500 * time.Millisecond, "1.5s"},
		{"duration 微秒", 250 * time.Microsecond, "250µs"},
	}
	for _, tc := range cases {
		got := logfmt.Encode(nil, []logfmt.Field{{Key: "k", Value: tc.value}})
		if want := "k=" + tc.want; string(got) != want {
			t.Fatalf("%s: Encode = %q, want %q", tc.name, got, want)
		}
	}
}

func TestEncodeAppendSemantics(t *testing.T) {
	fields := []logfmt.Field{
		{Key: "a", Value: int64(1)},
		{Key: "b", Value: "two"},
	}

	if got := logfmt.Encode(nil, fields); string(got) != "a=1 b=two" {
		t.Fatalf("Encode(nil, …) = %q", got)
	}

	dst := make([]byte, 0, 64)
	dst = append(dst, "HEAD"...)
	got := logfmt.Encode(dst, fields)
	if want := "HEAD a=1 b=two"; string(got) != want {
		t.Fatalf("Encode 追加 = %q, want %q", got, want)
	}

	// 追加不得覆盖 dst 已有的字节。
	if !bytes.HasPrefix(got, []byte("HEAD")) {
		t.Fatalf("dst 前缀被覆盖: %q", got)
	}
}

func TestStringMatchesEncode(t *testing.T) {
	sets := [][]logfmt.Field{
		nil,
		{{Key: "only", Value: "v"}},
		{{Key: "a", Value: int64(1)}, {Key: "b", Value: true}},
		{{Key: "x", Value: "sp ace"}, {Key: "x", Value: "last"}, {Key: "y", Value: 2.5}},
		{{Key: "u", Value: "日本"}, {Key: "bad", Value: []byte{0x80}}},
	}
	for i, fs := range sets {
		want := string(logfmt.Encode(nil, fs))
		if got := logfmt.String(fs); got != want {
			t.Fatalf("第 %d 组: String = %q, Encode = %q", i, got, want)
		}
	}
}

func TestValidateReportsEmptyKey(t *testing.T) {
	if err := logfmt.Validate(nil); err != nil {
		t.Fatalf("Validate(nil) = %v", err)
	}
	if err := logfmt.Validate([]logfmt.Field{}); err != nil {
		t.Fatalf("Validate([]) = %v", err)
	}
	if err := logfmt.Validate([]logfmt.Field{{Key: "ok", Value: int64(1)}}); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	err := logfmt.Validate([]logfmt.Field{{Key: "ok"}, {Key: "", Value: int64(2)}})
	if !errors.Is(err, logfmt.ErrEmptyKey) {
		t.Fatalf("空 key 时 Validate = %v, want ErrEmptyKey", err)
	}
}

func TestEncodeDoesNotMutateFields(t *testing.T) {
	fields := []logfmt.Field{
		{Key: "b", Value: int64(1)},
		{Key: "a", Value: []byte("xy")},
		{Key: "b", Value: "last"},
		{Key: "d", Value: 1.5},
	}

	before := make([]string, len(fields))
	for i := range fields {
		before[i] = fields[i].Key + "|" + valueRepr(fields[i].Value)
	}

	_ = logfmt.Encode(nil, fields)

	for i := range fields {
		if got := fields[i].Key + "|" + valueRepr(fields[i].Value); got != before[i] {
			t.Fatalf("第 %d 个字段被改动：%q → %q", i, before[i], got)
		}
	}
}

func TestConcurrentEncodeMatchesSingleThread(t *testing.T) {
	fields := []logfmt.Field{
		{Key: "app", Value: "gateway"},
		{Key: "n", Value: int64(7)},
		{Key: "ok", Value: true},
		{Key: "msg", Value: "hello world"},
		{Key: "n", Value: int64(9)},
		{Key: "took", Value: 1500 * time.Millisecond},
	}
	want := logfmt.String(fields)

	const workers, rounds = 8, 200
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		bad []string
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if got := logfmt.String(fields); got != want {
					mu.Lock()
					bad = append(bad, fmt.Sprintf("第 %d 次: %q", i, got))
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(bad) > 0 {
		t.Fatalf("并发结果不一致（期望 %q）：%v", want, bad[0])
	}
}

func valueRepr(v any) string {
	switch x := v.(type) {
	case []byte:
		return "bytes:" + string(x)
	default:
		return fmt.Sprintf("%T:%v", v, v)
	}
}
