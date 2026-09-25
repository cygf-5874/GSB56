package logfmt

import "errors"

// ErrEmptyKey 表示某个字段的 key 是空字符串。
//
// 空 key 没法编成 `k=v`，所以 Validate 会把它单独报出来。
var ErrEmptyKey = errors.New("logfmt: empty key")
