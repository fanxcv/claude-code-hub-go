package usagefeed

import "errors"

// ErrSubscriptionClosed 表示订阅已被关闭（或 Hub 已停），Wait 不再会返回信号。
var ErrSubscriptionClosed = errors.New("usagefeed: 订阅已关闭")
