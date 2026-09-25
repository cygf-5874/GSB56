# logfmt

把结构化字段编码成 logfmt 文本（`k=v` 以单个空格分隔）的 Go 库，仅标准库。

```go
line := logfmt.String([]logfmt.Field{
    {Key: "app", Value: "gateway"},
    {Key: "took", Value: 1500 * time.Millisecond},
    {Key: "msg", Value: "hello world"},
})
// app=gateway took=1.5s msg=hello\x20world
```

## 怎么跑

```bash
go test ./...                     # 既有用例
go test -race ./...               # 并发用例（需要 cgo 工具链）
go run ./logcheck                 # 验收场景（固定件）
go run ./logcheck -list
go run ./logcheck -only alloc
```

## 对外契约

1. **输出形状**：`k=v`，字段之间用单个空格分隔。值里出现 `\`、`"`、`\n`、`\r`、`\t`、
   空格、`=` 以及其它 `< 0x20` 的字节时，按下面写死的转义表处理。
2. **非法 UTF-8**：值里不构成合法 UTF-8 序列的字节，**逐字节**写成 `\xNN`（小写十六进制），
   不得替换成 `?`，也不得丢弃；合法的多字节序列原样保留。
3. **字段顺序**：按 key **首次出现**的顺序输出。同一个 key 出现多次时，
   值取**最后一次**出现的值，位置取**首次**出现的位置。
4. **追加语义**：`Encode(dst []byte, fields []Field) []byte` 把编码结果追加到 `dst`，
   不覆盖 `dst` 已有内容。`dst` 非空时第一个字段之前补一个空格；
   `fields` 为空时原样返回 `dst`。
5. `String(fields []Field) string` 的输出必须与 `string(Encode(nil, fields))` 逐字节一致。
6. **值类型与格式**：按下面的格式表输出。
7. **空 key**：`Validate(fields)` 遇到 `Key == ""` 时返回 `ErrEmptyKey`（`errors.go`）；
   `fields` 为 `nil` 时 `Encode(nil, nil)` 的长度是 0。
8. **分配预算**：见「分配预算」一节。
9. **吞吐**：见「吞吐」一节。
10. **并发**：`Encode` / `String` / `Validate` 可以被任意多个 goroutine 同时调用，
    实现里不得有包级可变状态。8 个 goroutine 各跑 1000 次，结果必须与单线程逐字节一致；
    `go test -race ./...` 干净。

### 值类型格式表

| Value 的静态类型 | 输出 |
| --- | --- |
| `string` | 内容（按转义表转义） |
| `[]byte` | 内容（按转义表转义） |
| `int64` | 十进制，等价于 `strconv.FormatInt(v, 10)` |
| `float64` | 等价于 `strconv.FormatFloat(v, 'g', -1, 64)` |
| `bool` | `true` / `false` |
| `time.Duration` | 与 `time.Duration.String()` 逐字节一致（`1.5s`、`250µs`、`2h3m0s`、`0s`） |
| 其它 | 等价于 `fmt.Sprint(v)`（不保证零分配） |

### 转义表（逐字节，只作用于值；key 原样输出不转义）

| 输入字节 | 输出 |
| --- | --- |
| `\`（0x5C） | `\\` |
| `"`（0x22） | `\"` |
| 0x0A | `\n` |
| 0x0D | `\r` |
| 0x09 | `\t` |
| 空格（0x20） | `\x20` |
| `=`（0x3D） | `\x3d` |
| 其它 `< 0x20` 的字节 | `\xNN`（小写十六进制） |
| `0x80`..`0xFF` 且不构成合法 UTF-8 序列 | `\xNN`（小写十六进制，逐字节） |
| 其它 | 原样输出 |

## 分配预算

`Encode(dst, fields)` 的返回值就是 `dst` 的延长。输出字节全部落在**调用方提供的那块内存**里，
所以判重、遍历、写转义、格式化数字每一步都可以直接 `append` 到 `dst` 上，不需要任何中间缓冲。
**只要 `dst` 的容量不小于最终长度，堆分配次数的理论下限就是 0。**
反过来，任何「先把字段攒进 `map` / `[]string`，或者先写进一个 `strings.Builder` 再拷回 `dst`」
的写法，都会在 `dst` 之外再至少分配一次。

`String(fields)` 的返回类型是 `string`，而 Go 里 `[]byte` → `string` 的转换会复制一份，于是：

- 先让 `Encode(nil, …)` 自己从 nil 增长、最后再 `string(...)`：增长过程本身就是若干次分配
  （8 → 16 → 32 …），再加最后一次拷贝，远不止 1 次；
- 先 `make([]byte, 0, n)` 再 `string(buf)`：`make` 1 次 + 拷贝 1 次 = 2 次；
- 要**恰好 1 次**，返回值必须直接引用唯一那块内存，也就是「只分配一次 → 原地写 → 借用」
  而不是「分配一次 → 写 → 再分配一次拷贝」。

所以本题的预算写成：**`Encode` 在 `dst` 容量足够时 0 次；`String` 恰好 1 次**
（`testing.AllocsPerRun` 口径，字段数 8 与 64 都必须满足）。`String` 的 1 次是理论下限。

> 现状：`Encode` 走的是「`map[string]string` 去重 + 全量 `sort` + 每字段 `fmt.Sprintf` +
> `strings.Builder`」的写法，本机实测 8 字段 14 次、64 字段 67 次分配；
> `String` 实测 16 次 / 69 次。

## 吞吐

判据是「64 字段编码 10000 次」的墙钟耗时（best of 5，避免偶发抖动）：`Encode` 与 `String`
各 10000 次的预算都是 **25 ms**。

标定过程：本机（Go 1.24.13 / windows/amd64）上，一份满足上面全部契约的实现实测
`Encode` 64×10000 ≈ 9.0 ms、`String` 64×10000 ≈ 9.4 ms；起点现状实测 ≈ 46 ms 与 ≈ 48 ms。
阈值 25 ms 相对参考量级留了约 2.8 倍余量。

判据依赖机器速度，**跑题机的机器更慢时按同一比例放宽**：只要实现的耗时仍在
「本机参考量级 × 2.8」以内即可。

## 验收

`logcheck/` 是固定验收程序，**不要修改**。场景分四组：

| 组 | 场景 | 覆盖 |
| --- | --- | --- |
| `format` | `FMT1_field_order_and_dedup` | 契约 3：首次出现顺序、重复 key 取末值与首位 |
| `format` | `FMT2_append_semantics` | 契约 4/5：追加不覆盖 `dst`、空字段、`String` 等价 |
| `format` | `FMT3_types_and_edges` | 契约 6/7：格式表、`Validate` 的空 key |
| `escape` | `ESC1_escape_table` | 契约 1：转义表逐字节 |
| `escape` | `ESC2_invalid_utf8_per_byte` | 契约 2：非法 UTF-8 逐字节 `\xNN` |
| `escape` | `ESC3_binary_escape_alloc_bound` | 契约 1/2 + 8：4096 字节二进制串转义正确且不额外分配 |
| `alloc` | `AL1_encode_zero_alloc` | 契约 8：`Encode` 0 次（8 与 64 字段） |
| `alloc` | `AL2_string_one_alloc` | 契约 8：`String` 1 次（8 与 64 字段） |
| `throughput` | `THR1_encode64_x10000` | 契约 9：`Encode` 64 字段 × 10000 次 < 25 ms |
| `throughput` | `THR2_string64_x10000` | 契约 9：`String` 64 字段 × 10000 次 < 25 ms |

`-only format|escape|alloc|throughput` 可以只跑一组。每个场景在独立子进程里执行，
挂死不会遮蔽其余场景；子进程带 25 秒看门狗。判据用的期望值由固定件自带的独立编码器算出，
不依赖被测代码。

## 目录

```
.
├── go.mod            module logfmt（无 require）
├── errors.go         ErrEmptyKey
├── logfmt.go         Field / Validate / Encode / String
├── logfmt_test.go    既有用例
├── logcheck/main.go  固定验收程序（勿改）
└── README.md
```
