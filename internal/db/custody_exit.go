package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
)

// CustodyExit is the audited record of an acknowledged custody exit from a
// terminal run whose preserved head and submitted head diverged without a
// containment proof (issue #233). It names which head the branch kept, which
// head was set aside, the exact head the operator acknowledged, the refs that
// keep the set-aside commits reachable, and every commit each head lacks, so a
// later reader sees the decision and what it left off the branch.
type CustodyExit struct {
	Mode          string              `json:"mode"`
	KeptHead      string              `json:"kept_head"`
	SetAsideHead  string              `json:"set_aside_head"`
	Acknowledged  string              `json:"acknowledged"`
	Anchors       []string            `json:"anchors"`
	LocalOnly     []CustodyExitCommit `json:"local_only"`
	PreservedOnly []CustodyExitCommit `json:"preserved_only"`
	// LocalOnlyTotal and PreservedOnlyTotal count every commit on each side;
	// the lists stop at a bound, and these say how many were not listed.
	LocalOnlyTotal     int    `json:"local_only_total"`
	PreservedOnlyTotal int    `json:"preserved_only_total"`
	Invoker            string `json:"invoker"`
	RecordedAt         int64  `json:"recorded_at"`
}

// CustodyExitCommit names one commit a head lacks. PatchID and PatchMatch are
// a lead for the reader, never a containment proof: a stable patch identity
// discards hunk locations and whitespace.
type CustodyExitCommit struct {
	SHA        string `json:"sha"`
	Subject    string `json:"subject"`
	PatchID    string `json:"patch_id,omitempty"`
	PatchMatch bool   `json:"patch_match"`
}

// ReturnRunCustodyWithExit returns custody of one run and records its exit in
// the same statement. It changes only a run whose custody is still held, so a
// repeated or concurrent exit can never write a second record; that case
// returns an error and leaves the first record untouched.
func (d *DB) ReturnRunCustodyWithExit(id string, exit CustodyExit) error {
	ts := now()
	exit.RecordedAt = ts
	payload, err := json.Marshal(exit)
	if err != nil {
		return fmt.Errorf("encode custody exit: %w", err)
	}
	result, err := sqlitewrite.Exec(context.Background(), d.sql, "ReturnRunCustodyWithExit",
		`UPDATE runs SET custody_returned_at = ?, custody_exit = ?, updated_at = ? WHERE id = ? AND custody_returned_at IS NULL`,
		ts, string(payload), ts, id)
	if err != nil {
		return fmt.Errorf("return run custody with exit: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("return run custody with exit: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("run %s no longer holds custody", id)
	}
	return nil
}

// GetRunCustodyExit returns the run's recorded custody exit, or nil when the
// run never took one.
func (d *DB) GetRunCustodyExit(id string) (*CustodyExit, error) {
	var stored sql.NullString
	if err := d.sql.QueryRow(`SELECT custody_exit FROM runs WHERE id = ?`, id).Scan(&stored); err != nil {
		return nil, fmt.Errorf("get run custody exit: %w", err)
	}
	if !stored.Valid {
		return nil, nil
	}
	var exit CustodyExit
	if err := json.Unmarshal([]byte(stored.String), &exit); err != nil {
		return nil, fmt.Errorf("decode run custody exit: %w", err)
	}
	return &exit, nil
}
