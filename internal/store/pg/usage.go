package pg

import (
	"context"
	"fmt"
	"time"
)

// DayUsage 是一天里全部模型调用的用量合计。
type DayUsage struct {
	Calls            int64
	PromptTokens     int64
	CachedTokens     int64
	CompletionTokens int64
	CostMicros       int64
}

// Tokens 是输入与输出 token 的合计。命中缓存的输入算在输入里。
func (u DayUsage) Tokens() int64 { return u.PromptTokens + u.CompletionTokens }

// AddUsage 把一次模型调用的用量累加到 day 那一天（按 day 所在时区的日期）。
func (s *Store) AddUsage(ctx context.Context, day time.Time, u DayUsage) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO llm_usage (day, calls, prompt_tokens, cached_tokens, completion_tokens, cost_micros)
		VALUES ($1::date, $2, $3, $4, $5, $6)
		ON CONFLICT (day) DO UPDATE SET
			calls = llm_usage.calls + EXCLUDED.calls,
			prompt_tokens = llm_usage.prompt_tokens + EXCLUDED.prompt_tokens,
			cached_tokens = llm_usage.cached_tokens + EXCLUDED.cached_tokens,
			completion_tokens = llm_usage.completion_tokens + EXCLUDED.completion_tokens,
			cost_micros = llm_usage.cost_micros + EXCLUDED.cost_micros`,
		day.Format(time.DateOnly), u.Calls, u.PromptTokens, u.CachedTokens, u.CompletionTokens, u.CostMicros)
	if err != nil {
		return fmt.Errorf("pg: 记录模型用量失败: %w", err)
	}
	return nil
}

// UsageOn 返回 day 那一天的用量。没有记录时是零值。
func (s *Store) UsageOn(ctx context.Context, day time.Time) (DayUsage, error) {
	var u DayUsage
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(calls), 0), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(cached_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(cost_micros), 0)
		FROM llm_usage WHERE day = $1::date`, day.Format(time.DateOnly)).
		Scan(&u.Calls, &u.PromptTokens, &u.CachedTokens, &u.CompletionTokens, &u.CostMicros)
	if err != nil {
		return u, fmt.Errorf("pg: 读取模型用量失败: %w", err)
	}
	return u, nil
}
