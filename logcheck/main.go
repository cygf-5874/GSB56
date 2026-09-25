// Command logcheck 是 logfmt 的固定验收程序。
//
// ⚠️ 不要修改本文件。它是判定「题目有没有做对」的依据；
// 修改它只会让判定失效，不会让实现变对。
//
// 10 个场景分四组：
//
//	format     3 个：字段顺序与重复 key / 追加语义 / 类型与边界
//	escape     3 个：转义表 / 非法 UTF-8 逐字节 / 二进制转义的分配上界
//	alloc      2 个：Encode 零分配（dst 容量足够）/ String 恰好一次分配
//	throughput 2 个：64 字段 Encode 与 String 各 10000 次
//
// 用法：
//
//	go run ./logcheck              # 跑全部场景
//	go run ./logcheck -only alloc  # 只跑一组
//	go run ./logcheck -list        # 列出场景
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"logfmt"
)

const (
	childTimeout = 25 * time.Second
	parentGrace  = 10 * time.Second

	// alloc 判据（testing.AllocsPerRun 口径）。
	// 阈值按参考实现实测值标定：Encode 实测 0 次、String 实测 1 次。
	maxEncodeAllocs = 0.0
	maxStringAllocs = 1.0
	allocRuns       = 1000

	// throughput 判据（按本机标定；跑题机慢按比例放宽）。见 README「吞吐」一节：
	// 参考实现实测 Encode 64×10000 ≈ 9 ms、String ≈ 9.4 ms；阈值取 25 ms（约 2.8 倍余量）。
	throughputIters  = 10000
	throughputRounds = 5
	thrEncodeBudget  = 25 * time.Millisecond
	thrStringBudget  = 25 * time.Millisecond
)

type scenario struct {
	Group string
	Name  string
	Run   func() error
}

var scenarios = []scenario{
	{"format", "FMT1_field_order_and_dedup", fieldOrderAndDedup},
	{"format", "FMT2_append_semantics", appendSemantics},
	{"format", "FMT3_types_and_edges", typesAndEdges},
	{"escape", "ESC1_escape_table", escapeTable},
	{"escape", "ESC2_invalid_utf8_per_byte", invalidUTF8},
	{"escape", "ESC3_binary_escape_alloc_bound", binaryEscapeAllocBound},
	{"alloc", "AL1_encode_zero_alloc", encodeZeroAlloc},
	{"alloc", "AL2_string_one_alloc", stringOneAlloc},
	{"throughput", "THR1_encode64_x10000", encodeThroughput},
	{"throughput", "THR2_string64_x10000", stringThroughput},
}

func main() {
	only := flag.String("only", "", "只跑指定组：format / escape / alloc / throughput")
	name := flag.String("scenario", "", "内部使用：只跑单个场景")
	list := flag.Bool("list", false, "列出全部场景")
	flag.Parse()

	if *list {
		for _, sc := range scenarios {
			fmt.Printf("%-10s %s\n", sc.Group, sc.Name)
		}
		return
	}
	if *name != "" {
		os.Exit(runChild(*name))
	}
	os.Exit(runParent(*only))
}

func runChild(name string) int {
	var sc *scenario
	for i := range scenarios {
		if scenarios[i].Name == name {
			sc = &scenarios[i]
			break
		}
	}
	if sc == nil {
		fmt.Fprintf(os.Stderr, "未知场景 %q\n", name)
		return 2
	}

	watchdog := time.AfterFunc(childTimeout, func() {
		fmt.Fprintf(os.Stderr, "看门狗：场景 %s 超过 %s 仍未结束，下面是全部 goroutine 栈\n",
			sc.Name, childTimeout)
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		os.Exit(3)
	})
	defer watchdog.Stop()

	start := time.Now()
	if err := sc.Run(); err != nil {
		fmt.Printf("FAIL (%s): %v\n", time.Since(start).Round(time.Millisecond), err)
		return 1
	}
	fmt.Printf("OK (%s)\n", time.Since(start).Round(time.Millisecond))
	return 0
}

func runParent(only string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("无法定位自身可执行文件:", err)
		return 1
	}

	groups := []string{"format", "escape", "alloc", "throughput"}
	if only != "" {
		found := false
		for _, g := range groups {
			if g == only {
				found = true
			}
		}
		if !found {
			fmt.Printf("未知分组 %q（可选：format / escape / alloc / throughput）\n", only)
			return 2
		}
		groups = []string{only}
	}

	total, passed := 0, 0
	for _, g := range groups {
		fmt.Printf("== 组 %s ==\n", g)
		for _, sc := range scenarios {
			if sc.Group != g {
				continue
			}
			total++

			ctx, cancel := context.WithTimeout(context.Background(), childTimeout+parentGrace)
			cmd := exec.CommandContext(ctx, exe, "-scenario", sc.Name)
			cmd.WaitDelay = 5 * time.Second
			out, runErr := cmd.CombinedOutput()
			cancel()

			text := strings.TrimSpace(string(out))
			if runErr == nil {
				passed++
				fmt.Printf("  PASS  %s/%s\n", sc.Group, sc.Name)
				continue
			}
			fmt.Printf("  FAIL  %s/%s  (%v)\n", sc.Group, sc.Name, runErr)
			for _, line := range tailLines(text, 12) {
				fmt.Printf("        | %s\n", line)
			}
		}
	}

	fmt.Println()
	fmt.Printf("结果：通过 %d/%d\n", passed, total)
	if passed != total {
		return 1
	}
	return 0
}

func tailLines(s string, n int) []string {
	if s == "" {
		return []string{"(子进程没有输出)"}
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append([]string{"…"}, lines[len(lines)-n:]...)
	}
	return lines
}

// ------------------------------------------------- 与实现无关的独立参考实现

// refEncode 是本文件自带的 logfmt 编码器（map + strings.Join）。
// 判据用它当独立预言机，所以不依赖被测代码。
func refEncode(fields []logfmt.Field) string {
	type kv struct{ k, v string }

	index := map[string]int{}
	pairs := make([]kv, 0, len(fields))
	for _, f := range fields {
		if i, ok := index[f.Key]; ok {
			pairs[i].v = refValue(f.Value)
			continue
		}
		index[f.Key] = len(pairs)
		pairs = append(pairs, kv{k: f.Key, v: refValue(f.Value)})
	}

	var b strings.Builder
	for i := range pairs {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(pairs[i].k)
		b.WriteByte('=')
		b.WriteString(pairs[i].v)
	}
	return b.String()
}

// refEncodeInto 是「追加到 dst」的期望值：dst 非空时先补一个空格。
func refEncodeInto(dst []byte, fields []logfmt.Field) []byte {
	if len(fields) == 0 {
		return dst
	}
	out := append([]byte{}, dst...)
	if len(dst) > 0 {
		out = append(out, ' ')
	}
	return append(out, refEncode(fields)...)
}

func refValue(v any) string {
	switch x := v.(type) {
	case string:
		return string(refEscape(nil, []byte(x)))
	case []byte:
		return string(refEscape(nil, x))
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

// refEscape 是 README 里那张写死的转义表的独立实现。
func refEscape(dst []byte, s []byte) []byte {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '\\':
				dst = append(dst, '\\', '\\')
			case '"':
				dst = append(dst, '\\', '"')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case ' ', '=':
				dst = refHex(dst, c)
			default:
				if c < 0x20 {
					dst = refHex(dst, c)
				} else {
					dst = append(dst, c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		if r == utf8.RuneError && size <= 1 {
			dst = refHex(dst, c)
			i++
			continue
		}
		dst = append(dst, s[i:i+size]...)
		i += size
	}
	return dst
}

func refHex(dst []byte, c byte) []byte {
	const digits = "0123456789abcdef"
	return append(dst, '\\', 'x', digits[c>>4], digits[c&0x0f])
}

func checkEncode(dst []byte, fields []logfmt.Field) error {
	got := logfmt.Encode(dst, fields)
	want := refEncodeInto(dst, fields)
	if !bytes.Equal(got, want) {
		return fmt.Errorf("Encode(%q, %v 个字段) = %q，期望 %q", dst, len(fields), got, want)
	}
	return nil
}

// ------------------------------------------------------------------- format

func fieldOrderAndDedup() error {
	cases := []struct {
		name   string
		fields []logfmt.Field
		want   string
	}{
		{"首次出现序不是字典序", []logfmt.Field{
			{Key: "zeta", Value: int64(1)},
			{Key: "alpha", Value: int64(2)},
			{Key: "mid", Value: int64(3)},
		}, "zeta=1 alpha=2 mid=3"},
		{"乱序插入 + 重复", []logfmt.Field{
			{Key: "b", Value: int64(1)},
			{Key: "a", Value: int64(2)},
			{Key: "b", Value: int64(3)},
			{Key: "a", Value: int64(4)},
			{Key: "c", Value: int64(5)},
		}, "b=3 a=4 c=5"},
		{"同一 key 连出三次", []logfmt.Field{
			{Key: "x", Value: "1"},
			{Key: "x", Value: "2"},
			{Key: "x", Value: "3"},
		}, "x=3"},
		{"重复 key 的值类型可以不同", []logfmt.Field{
			{Key: "dup", Value: "old"},
			{Key: "other", Value: int64(2)},
			{Key: "dup", Value: true},
		}, "dup=true other=2"},
		{"重复 key 位置取首次", []logfmt.Field{
			{Key: "p", Value: int64(1)},
			{Key: "q", Value: int64(2)},
			{Key: "r", Value: int64(3)},
			{Key: "p", Value: int64(9)},
		}, "p=9 q=2 r=3"},
	}

	for _, tc := range cases {
		if err := checkEncode(nil, tc.fields); err != nil {
			return fmt.Errorf("%s: %v", tc.name, err)
		}
		if got := logfmt.String(tc.fields); got != tc.want {
			return fmt.Errorf("%s: String = %q，期望 %q", tc.name, got, tc.want)
		}
	}
	return nil
}

func appendSemantics() error {
	fields := []logfmt.Field{
		{Key: "a", Value: int64(1)},
		{Key: "b", Value: "two"},
	}
	base := refEncode(fields)

	if got := logfmt.Encode(nil, fields); string(got) != base {
		return fmt.Errorf("Encode(nil, …) = %q，期望 %q", got, base)
	}

	prefixes := []string{"H", "HEAD", "HEAD ", "x\ny"}
	for _, p := range prefixes {
		dst := []byte(p)
		before := append([]byte{}, dst...)
		got := logfmt.Encode(dst, fields)

		if !bytes.HasPrefix(got, before) {
			return fmt.Errorf("dst=%q 的前缀被覆盖：%q", p, got)
		}
		if want := string(before) + " " + base; string(got) != want {
			return fmt.Errorf("dst=%q 追加结果 = %q，期望 %q", p, got, want)
		}
	}

	// dst 非空、fields 为空：原样返回，不得多补空格。
	if got := logfmt.Encode([]byte("KEEP"), nil); string(got) != "KEEP" {
		return fmt.Errorf("Encode(\"KEEP\", nil) = %q，期望 \"KEEP\"", got)
	}
	// dst 为 nil、fields 为空：长度为 0。
	if got := logfmt.Encode(nil, nil); len(got) != 0 {
		return fmt.Errorf("Encode(nil, nil) = %q，长度应为 0", got)
	}
	if got := logfmt.String(nil); got != "" {
		return fmt.Errorf("String(nil) = %q，期望 \"\"", got)
	}
	return nil
}

func typesAndEdges() error {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"string", "plain", "plain"},
		{"bytes", []byte("raw-bytes"), "raw-bytes"},
		{"int64 正", int64(8443), "8443"},
		{"int64 负", int64(-1), "-1"},
		{"int64 极值", int64(-9223372036854775808), "-9223372036854775808"},
		{"float64", 0.125, "0.125"},
		{"float64 整数", 3.0, "3"},
		{"float64 负指数", 1e-7, "1e-07"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"duration 秒", 1500 * time.Millisecond, "1.5s"},
		{"duration 微秒", 250 * time.Microsecond, "250µs"},
		{"duration 零", time.Duration(0), "0s"},
		{"duration 负", -3 * time.Second, "-3s"},
		{"duration 小时", 2*time.Hour + 3*time.Minute, "2h3m0s"},
	}
	for _, tc := range cases {
		fields := []logfmt.Field{{Key: "k", Value: tc.value}}
		if err := checkEncode(nil, fields); err != nil {
			return fmt.Errorf("%s: %v", tc.name, err)
		}
		if got := logfmt.String(fields); got != "k="+tc.want {
			return fmt.Errorf("%s: String = %q，期望 %q", tc.name, got, "k="+tc.want)
		}
	}

	// 混合类型的一组，整体字节比对。
	mixed := []logfmt.Field{
		{Key: "s", Value: "str"},
		{Key: "b", Value: []byte("bytes")},
		{Key: "i", Value: int64(42)},
		{Key: "f", Value: 2.5},
		{Key: "o", Value: true},
		{Key: "d", Value: 90 * time.Second},
	}
	if err := checkEncode(nil, mixed); err != nil {
		return err
	}

	// Validate：空 key 报 ErrEmptyKey。
	if err := logfmt.Validate(nil); err != nil {
		return fmt.Errorf("Validate(nil) = %v", err)
	}
	if err := logfmt.Validate([]logfmt.Field{}); err != nil {
		return fmt.Errorf("Validate(空切片) = %v", err)
	}
	if err := logfmt.Validate(mixed); err != nil {
		return fmt.Errorf("Validate(混合字段) = %v", err)
	}
	err := logfmt.Validate([]logfmt.Field{{Key: "ok", Value: int64(1)}, {Key: ""}})
	if !errors.Is(err, logfmt.ErrEmptyKey) {
		return fmt.Errorf("空 key 时 Validate = %v，期望 ErrEmptyKey", err)
	}
	return nil
}

// ------------------------------------------------------------------- escape

func escapeTable() error {
	cases := []struct {
		name  string
		value string
	}{
		{"反斜杠", `a\b`},
		{"双引号", `say "hi"`},
		{"换行", "line1\nline2"},
		{"回车", "cr\r"},
		{"制表", "a\tb"},
		{"空格", "a b c"},
		{"等号", "a=b=c"},
		{"低位控制字节", "\x00\x01\x08\x0b\x0c\x1f"},
		{"混合", "k=v \"q\" \\ \n \t \x02"},
		{"原样保留", "AZaz09_.-/:@+*%#~[]{}()<>|,;!?'`"},
		{"全表", "a b=c\"d\\e\n\r\t\x01\x1f"},
	}
	for _, tc := range cases {
		fields := []logfmt.Field{{Key: "k", Value: tc.value}}
		if err := checkEncode(nil, fields); err != nil {
			return fmt.Errorf("%s: %v", tc.name, err)
		}
		if got, want := logfmt.String(fields), "k="+string(refEscape(nil, []byte(tc.value))); got != want {
			return fmt.Errorf("%s: String = %q，期望 %q", tc.name, got, want)
		}
	}
	return nil
}

func invalidUTF8() error {
	values := [][]byte{
		{0xff},
		{0xff, 'a', 0xfe, 0x80},
		{0xc3},
		{0xe6, 0x97},
		{0xf0, 0x9f, 0x92},
		{0x80, 0x80, 0x80},
		[]byte("日\xc3"),
		[]byte("日本語"),
		[]byte("ok \xff"),
	}
	for i, v := range values {
		fields := []logfmt.Field{{Key: "k", Value: v}}
		if err := checkEncode(nil, fields); err != nil {
			return fmt.Errorf("第 %d 组: %v", i, err)
		}
		got := logfmt.String(fields)
		want := "k=" + string(refEscape(nil, v))
		if got != want {
			return fmt.Errorf("第 %d 组: String = %q，期望 %q", i, got, want)
		}
		if strings.Contains(got, "?") {
			return fmt.Errorf("第 %d 组: 出现 %q，非法字节不许替换成 '?'", i, got)
		}
	}

	// U+FFFD（EF BF BD）本身是合法编码，必须原样保留。
	fields := []logfmt.Field{{Key: "k", Value: "\ufffd"}}
	if err := checkEncode(nil, fields); err != nil {
		return err
	}
	return nil
}

// binaryBlob 是确定性的伪随机二进制串（含大量非法 UTF-8 与需要转义的字节）。
func binaryBlob(n int) []byte {
	rng := rand.New(rand.NewSource(20260925))
	out := make([]byte, n)
	// 0x00..0x1f、0x20、0x3d、0x22、0x5c、0x7f、0x80..0xff 都覆盖到
	alphabet := []byte{0x00, 0x01, 0x08, 0x09, 0x0a, 0x0d, 0x1f, 0x20, 0x3d, 0x22, 0x5c,
		0x41, 0x7f, 0x80, 0xc3, 0xe6, 0xf0, 0xff, 0xfe, 0x9f}
	for i := range out {
		out[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return out
}

// ESC3：大块二进制转义不但要逐字节正确，还必须直接写进 dst，不许为了转义再攒一份中间缓冲。
func binaryEscapeAllocBound() error {
	const blobLen = 4096
	blob := binaryBlob(blobLen)
	fields := []logfmt.Field{{Key: "blob", Value: blob}}

	want := refEncodeInto(nil, fields)
	dst := make([]byte, 0, len(want)+64)

	var out []byte
	got := logfmt.Encode(dst[:0], fields)
	if !bytes.Equal(got, want) {
		return fmt.Errorf("转义 %d 字节二进制串的结果不对：长度 %d，期望 %d", blobLen, len(got), len(want))
	}
	out = got

	perCall := testing.AllocsPerRun(200, func() {
		out = logfmt.Encode(dst[:0], fields)
	})
	if !bytes.Equal(out, want) {
		return errors.New("复用 dst 之后结果变了")
	}
	if perCall > maxEncodeAllocs {
		return fmt.Errorf("转义 %d 字节二进制串平均分配 %.2f 次，上限 %.1f", blobLen, perCall, maxEncodeAllocs)
	}
	return nil
}

// -------------------------------------------------------------------- alloc

// buildTypical 造 n 个字段，覆盖全部六种内置类型，并掺入需要转义的字节。
func buildTypical(n int) []logfmt.Field {
	fields := make([]logfmt.Field, 0, n)
	for i := 0; i < n; i++ {
		key := "f" + strconv.Itoa(i)
		switch i % 6 {
		case 0:
			fields = append(fields, logfmt.Field{Key: key, Value: "v" + strconv.Itoa(i) + " x"})
		case 1:
			fields = append(fields, logfmt.Field{Key: key, Value: int64(i) * 1000})
		case 2:
			fields = append(fields, logfmt.Field{Key: key, Value: float64(i) / 8})
		case 3:
			fields = append(fields, logfmt.Field{Key: key, Value: i%2 == 0})
		case 4:
			fields = append(fields, logfmt.Field{Key: key, Value: []byte("b" + strconv.Itoa(i))})
		case 5:
			fields = append(fields, logfmt.Field{Key: key, Value: time.Duration(i) * time.Millisecond})
		}
	}
	return fields
}

// AL1：dst 容量足够时 Encode 必须 0 次分配（8 与 64 个字段都要满足）。
func encodeZeroAlloc() error {
	for _, n := range []int{8, 64} {
		fields := buildTypical(n)
		want := refEncodeInto(nil, fields)
		dst := make([]byte, 0, len(want)+256)

		var out []byte
		got := testing.AllocsPerRun(allocRuns, func() {
			out = logfmt.Encode(dst[:0], fields)
		})
		if !bytes.Equal(out, want) {
			return fmt.Errorf("%d 字段：复用 dst 之后结果变了", n)
		}
		if got > maxEncodeAllocs {
			return fmt.Errorf("%d 字段：Encode 平均分配 %.2f 次，上限 %.1f", n, got, maxEncodeAllocs)
		}
	}
	return nil
}

// AL2：String 恰好分配 1 次（8 与 64 个字段都要满足）。
func stringOneAlloc() error {
	for _, n := range []int{8, 64} {
		fields := buildTypical(n)
		want := refEncode(fields)

		var s string
		got := testing.AllocsPerRun(allocRuns, func() {
			s = logfmt.String(fields)
		})
		if s != want {
			return fmt.Errorf("%d 字段：String 结果不对", n)
		}
		if got > maxStringAllocs {
			return fmt.Errorf("%d 字段：String 平均分配 %.2f 次，上限 %.1f", n, got, maxStringAllocs)
		}
	}
	return nil
}

// --------------------------------------------------------------- throughput

// bestOf 跑 rounds 轮，每轮 iters 次，返回最快的一轮耗时。
func bestOf(rounds int, iters int, step func()) time.Duration {
	best := time.Duration(1<<62 - 1)
	for r := 0; r < rounds; r++ {
		start := time.Now()
		for i := 0; i < iters; i++ {
			step()
		}
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}

// sinkEncode、sinkString 防止编译器把整轮循环优化掉。
var (
	sinkEncode []byte
	sinkString string
)

func encodeThroughput() error {
	fields := buildTypical(64)
	want := refEncodeInto(nil, fields)
	dst := make([]byte, 0, len(want)+256)

	best := bestOf(throughputRounds, throughputIters, func() {
		sinkEncode = logfmt.Encode(dst[:0], fields)
	})
	if !bytes.Equal(sinkEncode, want) {
		return errors.New("64 字段 Encode 结果不对")
	}
	if best > thrEncodeBudget {
		return fmt.Errorf("64 字段 Encode %d 次耗时 %v，预算 %v",
			throughputIters, best.Round(time.Millisecond), thrEncodeBudget)
	}
	return nil
}

func stringThroughput() error {
	fields := buildTypical(64)
	want := refEncode(fields)

	best := bestOf(throughputRounds, throughputIters, func() {
		sinkString = logfmt.String(fields)
	})
	if sinkString != want {
		return errors.New("64 字段 String 结果不对")
	}
	if best > thrStringBudget {
		return fmt.Errorf("64 字段 String %d 次耗时 %v，预算 %v",
			throughputIters, best.Round(time.Millisecond), thrStringBudget)
	}
	return nil
}
