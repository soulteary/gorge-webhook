package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type Store struct {
	db              *sql.DB
	errorBackoffSec int
	errorThreshold  int
}

func NewStore(dsn string, errorBackoffSec, errorThreshold int) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &Store{
		db:              db,
		errorBackoffSec: errorBackoffSec,
		errorThreshold:  errorThreshold,
	}, nil
}

func (s *Store) Close() {
	_ = s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) FetchQueuedRequests(ctx context.Context, limit int) ([]*WebhookRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, phid, webhookPHID, objectPHID, status, properties,
		        lastRequestResult, lastRequestEpoch, dateCreated, dateModified
		 FROM herald_webhookrequest
		 WHERE status = ?
		 ORDER BY id ASC
		 LIMIT ?`,
		StatusQueued, limit)
	if err != nil {
		return nil, fmt.Errorf("query queued requests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var requests []*WebhookRequest
	for rows.Next() {
		r := &WebhookRequest{}
		if err := rows.Scan(
			&r.ID, &r.PHID, &r.WebhookPHID, &r.ObjectPHID, &r.Status,
			&r.Properties, &r.LastRequestResult, &r.LastRequestEpoch,
			&r.DateCreated, &r.DateModified,
		); err != nil {
			return nil, fmt.Errorf("scan request: %w", err)
		}
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

func (s *Store) GetWebhook(ctx context.Context, phid string) (*Webhook, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, phid, name, webhookURI, status, hmacKey
		 FROM herald_webhook
		 WHERE phid = ?`,
		phid)

	w := &Webhook{}
	err := row.Scan(&w.ID, &w.PHID, &w.Name, &w.WebhookURI, &w.Status, &w.HmacKey)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan webhook: %w", err)
	}
	return w, nil
}

func (s *Store) IsInErrorBackoff(ctx context.Context, webhookPHID string) (bool, error) {
	cutoff := time.Now().Unix() - int64(s.errorBackoffSec)
	var count int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM herald_webhookrequest
		 WHERE webhookPHID = ?
		   AND lastRequestResult = ?
		   AND lastRequestEpoch >= ?`,
		webhookPHID, ResultFail, cutoff).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check backoff: %w", err)
	}
	return count >= int64(s.errorThreshold), nil
}

func (s *Store) UpdateRequestResult(ctx context.Context, reqID int64, status, result, errorType, errorCode string, epoch int64, props *WebhookRequestProperties) error {
	propsJSON, err := json.Marshal(props)
	if err != nil {
		return fmt.Errorf("marshal properties: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE herald_webhookrequest
		 SET status = ?, lastRequestResult = ?, lastRequestEpoch = ?,
		     properties = ?, dateModified = ?
		 WHERE id = ?`,
		status, result, epoch, string(propsJSON), time.Now().Unix(), reqID)
	if err != nil {
		return fmt.Errorf("update request: %w", err)
	}
	return nil
}

func (s *Store) Stats(ctx context.Context) (*DeliveryStats, error) {
	stats := &DeliveryStats{}

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM herald_webhookrequest WHERE status = ?`,
		StatusQueued).Scan(&stats.QueuedCount)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM herald_webhookrequest WHERE status = ?`,
		StatusSent).Scan(&stats.SentCount)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM herald_webhookrequest WHERE status = ?`,
		StatusFailed).Scan(&stats.FailedCount)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM herald_webhook WHERE status != 'disabled'`,
	).Scan(&stats.ActiveWebhooks)
	if err != nil {
		return nil, err
	}

	return stats, nil
}
