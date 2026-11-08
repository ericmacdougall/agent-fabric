package reviewgate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const durableResultTruncationMarker = "\n[output truncated]"

type pendingToolResult struct {
	output    string
	isError   bool
	truncated bool
}

// prepareToolResult applies the same redaction boundary as the agent loop,
// then bounds the redacted bytes. Raw executor output never reaches SQL.
func (g *Gate) prepareToolResult(result *ToolResult) *pendingToolResult {
	if result == nil {
		return nil
	}
	output := g.redactor.Redact(result.Output)
	truncated := false
	if len(output) > g.resultLimit {
		truncated = true
		keep := g.resultLimit - len(durableResultTruncationMarker)
		output = output[:keep] + durableResultTruncationMarker
	}
	return &pendingToolResult{output: output, isError: result.IsError, truncated: truncated}
}

// insertToolResult shares finishExecution's transaction and dispatch lock.
// Any error rolls the terminal request update back with this insert.
func insertToolResult(ctx context.Context, tx pgx.Tx, request storedRequest, outcome Outcome, result *pendingToolResult) error {
	if result == nil {
		return nil
	}
	if request.DispatchKey == nil {
		return fmt.Errorf("%w: terminal dispatch has no idempotency key", ErrConflict)
	}
	digest := hashBytes([]byte(result.output))
	_, err := tx.Exec(ctx, `INSERT INTO review_gate_results
		(request_hash, dispatch_fence, dispatch_idem_key, outcome, result_format, output, is_error, truncated, output_sha256)
		VALUES ($1,$2,$3,$4,'redacted-bounded-v1',$5,$6,$7,$8)`,
		request.Hash, request.Fence, *request.DispatchKey, outcome, []byte(result.output), result.isError, result.truncated, digest)
	return err
}

// LoadToolResult returns only a result whose request, outcome, fence and key
// still match. It performs no reviewer or executor call and cannot retry work.
func (g *Gate) LoadToolResult(ctx context.Context, requestHash string) (StoredToolResult, error) {
	if !validHash(requestHash) {
		return StoredToolResult{}, invalid("invalid review request hash")
	}
	var output []byte
	var result StoredToolResult
	var digest string
	err := g.store.pool.QueryRow(ctx, `SELECT v.output, v.is_error, v.truncated, v.output_sha256
		FROM review_gate_results v JOIN review_gate_requests r ON r.request_hash = v.request_hash
		WHERE v.request_hash = $1 AND r.state = v.outcome
		  AND r.dispatch_fence = v.dispatch_fence AND r.dispatch_idem_key = v.dispatch_idem_key`, requestHash).
		Scan(&output, &result.IsError, &result.Truncated, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredToolResult{}, fmt.Errorf("%w: durable result for %s", ErrNotFound, requestHash)
	}
	if err != nil {
		return StoredToolResult{}, err
	}
	if len(output) > MaxDurableResultBytes || hashBytes(output) != digest {
		return StoredToolResult{}, fmt.Errorf("%w: durable result fails its bound or digest", ErrConflict)
	}
	result.Output = string(output)
	return result, nil
}
