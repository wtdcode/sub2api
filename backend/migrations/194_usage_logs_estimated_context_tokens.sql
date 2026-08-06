-- Record the scheduler's prompt-length estimate so it can be compared with actual input_tokens.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS estimated_context_tokens INTEGER;
