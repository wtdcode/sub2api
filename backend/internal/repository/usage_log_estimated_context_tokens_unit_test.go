//go:build unit

package repository

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newEstimatedContextTokensUsageLog(estimated *int) *service.UsageLog {
	return &service.UsageLog{
		UserID:                 1,
		APIKeyID:               2,
		AccountID:              3,
		RequestID:              "req-estimated-context",
		Model:                  "gpt-5",
		InputTokens:            120,
		OutputTokens:           5,
		EstimatedContextTokens: estimated,
		CreatedAt:              time.Now().UTC(),
	}
}

// TestPrepareUsageLogInsert_EstimatedContextTokensArgWiring pins the
// estimated_context_tokens column to the arg slice / arg-type table so the
// INSERT column lists stay in sync. It sits between account_stats_cost and
// session_id (created_at is always last).
func TestPrepareUsageLogInsert_EstimatedContextTokensArgWiring(t *testing.T) {
	estimated := 131072
	prepared := prepareUsageLogInsert(newEstimatedContextTokensUsageLog(&estimated))
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	// created_at last, session_id penultimate, estimated_context_tokens before it.
	idx := len(prepared.args) - 3
	arg, ok := prepared.args[idx].(sql.NullInt64)
	require.True(t, ok, "estimated_context_tokens arg should be sql.NullInt64, got %T", prepared.args[idx])
	require.True(t, arg.Valid)
	require.Equal(t, int64(estimated), arg.Int64)
	require.Equal(t, "integer", usageLogInsertArgTypes[idx])
}

// TestPrepareUsageLogInsert_EstimatedContextTokensNullWhenAbsent proves an
// absent estimate is persisted as SQL NULL rather than zero.
func TestPrepareUsageLogInsert_EstimatedContextTokensNullWhenAbsent(t *testing.T) {
	prepared := prepareUsageLogInsert(newEstimatedContextTokensUsageLog(nil))
	arg, ok := prepared.args[len(prepared.args)-3].(sql.NullInt64)
	require.True(t, ok, "estimated_context_tokens arg should be sql.NullInt64, got %T", prepared.args[len(prepared.args)-3])
	require.False(t, arg.Valid, "absent estimate must be NULL, not zero")
}

// TestUsageLogQueries_IncludeEstimatedContextTokens guards that every generated
// INSERT path and the SELECT column list reference estimated_context_tokens.
func TestUsageLogQueries_IncludeEstimatedContextTokens(t *testing.T) {
	require.Contains(t, usageLogSelectColumns, "estimated_context_tokens")

	estimated := 4096
	log := newEstimatedContextTokensUsageLog(&estimated)
	prepared := prepareUsageLogInsert(log)
	key := usageLogBatchKey(log.RequestID, log.APIKeyID)

	batchQuery, batchArgs := buildUsageLogBatchInsertQuery([]string{key},
		map[string]usageLogInsertPrepared{key: prepared})
	// CTE input def + INSERT column list + SELECT ... FROM input.
	require.GreaterOrEqual(t, strings.Count(batchQuery, "estimated_context_tokens"), 3)
	require.Len(t, batchArgs, len(prepared.args)+1)

	bestEffortQuery, bestEffortArgs := buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared})
	require.GreaterOrEqual(t, strings.Count(bestEffortQuery, "estimated_context_tokens"), 3)
	require.Len(t, bestEffortArgs, len(prepared.args))
}
