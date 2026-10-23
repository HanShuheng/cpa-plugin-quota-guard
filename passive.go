package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// passiveSample 是一次被动额度观测：真实请求的响应头（usage.handle 回调）
// 携带完整的 X-Codex 额度信号，账号有真实流量时无需任何主动查询。
type passiveSample struct {
	At    time.Time
	Usage liveUsage
}

// observeUsage 处理 usage.handle：解析 codex 响应头里的额度信号并缓存。
func (g *Guard) observeUsage(record pluginapi.UsageRecord) {
	if record.Provider != "codex" || record.AuthIndex == "" || len(record.ResponseHeaders) == 0 {
		return
	}
	usage, ok := usageFromHeaders(record.ResponseHeaders)
	if !ok {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	g.mu.Lock()
	if g.passive == nil {
		g.passive = map[string]passiveSample{}
	}
	g.passive[record.AuthIndex] = passiveSample{At: at, Usage: usage}
	g.mu.Unlock()
}

// usageFromHeaders 把 X-Codex 响应头转换为额度读数。Primary 是 5 小时窗口，
// Secondary 是周窗口（与 CPA model_quotas 记录的信号一致）。
func usageFromHeaders(header http.Header) (liveUsage, bool) {
	pUsed, okP := headerNumber(header, "X-Codex-Primary-Used-Percent")
	sUsed, okS := headerNumber(header, "X-Codex-Secondary-Used-Percent")
	if !okP && !okS {
		return liveUsage{}, false
	}
	out := liveUsage{FiveHourRemaining: 100, WeeklyRemaining: 100}
	if okP {
		out.FiveHourRemaining = clamp(100 - pUsed)
		out.FiveHourReset = headerEpoch(header, "X-Codex-Primary-Reset-At")
	}
	if okS {
		out.WeeklyRemaining = clamp(100 - sUsed)
		out.WeeklyReset = headerEpoch(header, "X-Codex-Secondary-Reset-At")
	}
	return out, true
}

func headerNumber(header http.Header, name string) (float64, bool) {
	value := header.Get(name)
	if value == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func headerEpoch(header http.Header, name string) string {
	value := header.Get(name)
	if value == "" {
		return ""
	}
	epoch, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return value
	}
	return time.Unix(int64(epoch), 0).UTC().Format(time.RFC3339)
}
