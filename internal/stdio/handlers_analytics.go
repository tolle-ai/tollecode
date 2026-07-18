package stdio

import (
	"sort"
	"strings"

	"github.com/tolle-ai/tollecode/internal/session"
)

func handleGetAnalytics(state *ServerState, cmd map[string]any) {
	ws := workspaceFromCmd(state, cmd)
	sessions, _ := session.List(ws)

	// ── Session stats ─────────────────────────────────────────────────────────
	byStatus := map[string]int{}
	byMode := map[string]int{}
	byAgent := map[string]int{}
	sessPerDay := map[string]int{}
	totalMessages := 0

	for _, s := range sessions {
		st := s.Status
		if st == "" {
			st = "idle"
		}
		byStatus[st]++
		if s.Mode != "" {
			byMode[s.Mode]++
		}
		if s.AgentName != "" {
			byAgent[s.AgentName]++
		}
		day := s.CreatedAt
		if len(day) >= 10 {
			day = day[:10]
		}
		sessPerDay[day]++
		if s.MessageCount != nil {
			totalMessages += *s.MessageCount
		}
	}

	avgMsgs := 0.0
	if len(sessions) > 0 {
		avgMsgs = float64(totalMessages) / float64(len(sessions))
	}

	sessionStats := map[string]any{
		"total_sessions":            len(sessions),
		"total_messages":            totalMessages,
		"avg_messages_per_session":  avgMsgs,
		"by_status":                 mapToKV(byStatus, "status"),
		"by_mode":                   mapToKV(byMode, "mode"),
		"by_agent":                  mapToKV(byAgent, "agent"),
		"sessions_per_day":          dayKV(sessPerDay),
	}

	// ── Token usage from session headers ──────────────────────────────────────
	usage := aggregateUsage(ws)

	dailyAvg := map[string]any{
		"input_tokens": 0, "output_tokens": 0, "cost": 0.0, "calls": 0,
	}
	if dailyRows, ok := usage["daily"].([]map[string]any); ok && len(dailyRows) > 0 {
		n := len(dailyRows)
		var sumIn, sumOut int
		for _, d := range dailyRows {
			sumIn += d["input_tokens"].(int)
			sumOut += d["output_tokens"].(int)
		}
		dailyAvg = map[string]any{
			"input_tokens":  sumIn / n,
			"output_tokens": sumOut / n,
			"cost":          0.0,
			"calls":         len(sessions) / n,
		}
	}

	analytics := map[string]any{
		"totals":        usage["totals"],
		"daily":         usage["daily"],
		"by_provider":   usage["by_provider"],
		"by_model":      usage["by_model"],
		"daily_avg":     dailyAvg,
		"session_stats": sessionStats,
	}

	Emit(map[string]any{"type": "analytics", "data": analytics})
}

func mapToKV(m map[string]int, keyName string) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for k, v := range m {
		out = append(out, map[string]any{keyName: k, "count": v})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i][keyName].(string) < out[j][keyName].(string)
	})
	return out
}

func dayKV(m map[string]int) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for k, v := range m {
		out = append(out, map[string]any{"date": k, "count": v})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Compare(out[i]["date"].(string), out[j]["date"].(string)) < 0
	})
	return out
}
