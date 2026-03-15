package delivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Dispatcher struct {
	store         *Store
	client        *http.Client
	pollInterval  time.Duration
	maxConcurrent int
	backoffSec    int

	mu       sync.Mutex
	hookLock map[string]*sync.Mutex
}

func NewDispatcher(store *Store, timeoutSec, pollIntervalMs, maxConcurrent, backoffSec int) *Dispatcher {
	return &Dispatcher{
		store: store,
		client: &http.Client{
			Timeout: time.Duration(timeoutSec) * time.Second,
		},
		pollInterval:  time.Duration(pollIntervalMs) * time.Millisecond,
		maxConcurrent: maxConcurrent,
		backoffSec:    backoffSec,
		hookLock:      make(map[string]*sync.Mutex),
	}
}

func (d *Dispatcher) Run(ctx context.Context) {
	log.Printf("[webhook] dispatcher started, poll=%s, concurrency=%d",
		d.pollInterval, d.maxConcurrent)

	sem := make(chan struct{}, d.maxConcurrent)
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[webhook] dispatcher shutting down")
			return
		case <-ticker.C:
			requests, err := d.store.FetchQueuedRequests(ctx, d.maxConcurrent)
			if err != nil {
				log.Printf("[webhook] poll error: %v", err)
				continue
			}
			for _, req := range requests {
				sem <- struct{}{}
				go func(r *WebhookRequest) {
					defer func() { <-sem }()
					d.processRequest(ctx, r)
				}(req)
			}
		}
	}
}

func (d *Dispatcher) getLock(webhookPHID string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	lk, ok := d.hookLock[webhookPHID]
	if !ok {
		lk = &sync.Mutex{}
		d.hookLock[webhookPHID] = lk
	}
	return lk
}

func (d *Dispatcher) processRequest(ctx context.Context, req *WebhookRequest) {
	var props WebhookRequestProperties
	if err := json.Unmarshal([]byte(req.Properties), &props); err != nil {
		log.Printf("[webhook] failed to parse properties for request %s: %v", req.PHID, err)
		d.failPermanent(ctx, req, &props, ErrorTypeHook, "invalid-properties")
		return
	}

	hook, err := d.store.GetWebhook(ctx, req.WebhookPHID)
	if err != nil {
		log.Printf("[webhook] failed to load webhook %s: %v", req.WebhookPHID, err)
		return
	}
	if hook == nil {
		log.Printf("[webhook] webhook %s not found for request %s", req.WebhookPHID, req.PHID)
		d.failPermanent(ctx, req, &props, ErrorTypeHook, "not-found")
		return
	}

	if hook.Status == "disabled" {
		log.Printf("[webhook] webhook %s is disabled, failing request %s", hook.PHID, req.PHID)
		d.failPermanent(ctx, req, &props, ErrorTypeHook, "disabled")
		return
	}

	lk := d.getLock(hook.PHID)
	lk.Lock()
	defer lk.Unlock()

	inBackoff, err := d.store.IsInErrorBackoff(ctx, hook.PHID)
	if err != nil {
		log.Printf("[webhook] backoff check error for %s: %v", hook.PHID, err)
		return
	}
	if inBackoff {
		log.Printf("[webhook] webhook %s in error backoff, skipping request %s", hook.PHID, req.PHID)
		return
	}

	d.deliverRequest(ctx, req, hook, &props)
}

func (d *Dispatcher) deliverRequest(ctx context.Context, req *WebhookRequest, hook *Webhook, props *WebhookRequestProperties) {
	objectType := phidGetType(req.ObjectPHID)

	triggers := make([]PayloadTrigger, 0, len(props.TriggerPHIDs))
	for _, phid := range props.TriggerPHIDs {
		triggers = append(triggers, PayloadTrigger{PHID: phid})
	}

	transactions := make([]PayloadTransaction, 0, len(props.TransactionPHIDs))
	for _, phid := range props.TransactionPHIDs {
		transactions = append(transactions, PayloadTransaction{PHID: phid})
	}

	payload := WebhookPayload{
		Object: PayloadObject{
			Type: objectType,
			PHID: req.ObjectPHID,
		},
		Triggers: triggers,
		Action: PayloadAction{
			Test:   props.Test,
			Silent: props.Silent,
			Secure: props.Secure,
			Epoch:  req.DateCreated,
		},
		Transactions: transactions,
	}

	payloadBytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("[webhook] payload marshal error for %s: %v", req.PHID, err)
		return
	}
	payloadStr := string(payloadBytes) + "\n"

	signature := computeHMACSHA256(payloadStr, hook.HmacKey)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.WebhookURI, strings.NewReader(payloadStr))
	if err != nil {
		log.Printf("[webhook] build request error for %s: %v", req.PHID, err)
		d.handleFailure(ctx, req, props, ErrorTypeHook, "request-build-error", time.Now().Unix())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Phabricator-Webhook-Signature", signature)

	start := time.Now()
	resp, err := d.client.Do(httpReq)
	duration := time.Since(start)

	now := time.Now().Unix()

	if err != nil {
		var errorType, errorCode string
		if ctx.Err() != nil || isTimeoutErr(err) {
			errorType = ErrorTypeTimeout
			errorCode = "timeout"
		} else {
			errorType = ErrorTypeHTTP
			errorCode = err.Error()
		}

		log.Printf("[webhook] delivery failed: request=%s hook=%s uri=%s duration=%s error=%v",
			req.PHID, hook.PHID, hook.WebhookURI, duration, err)

		d.handleFailure(ctx, req, props, errorType, errorCode, now)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	statusCode := fmt.Sprintf("%d", resp.StatusCode)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("[webhook] delivered: request=%s hook=%s status=%d duration=%s",
			req.PHID, hook.PHID, resp.StatusCode, duration)
		props.ErrorType = ErrorTypeHTTP
		props.ErrorCode = statusCode
		_ = d.store.UpdateRequestResult(ctx, req.ID, StatusSent, ResultOkay, ErrorTypeHTTP, statusCode, now, props)
		return
	}

	log.Printf("[webhook] delivery error: request=%s hook=%s status=%d duration=%s",
		req.PHID, hook.PHID, resp.StatusCode, duration)
	d.handleFailure(ctx, req, props, ErrorTypeHTTP, statusCode, now)
}

func (d *Dispatcher) failPermanent(ctx context.Context, req *WebhookRequest, props *WebhookRequestProperties, errorType, errorCode string) {
	props.ErrorType = errorType
	props.ErrorCode = errorCode
	_ = d.store.UpdateRequestResult(ctx, req.ID, StatusFailed, ResultNone, errorType, errorCode, 0, props)
}

// handleFailure records a delivery failure. RETRY_FOREVER requests stay
// queued so the dispatcher will pick them up again; RETRY_NEVER requests
// are marked as permanently failed.
func (d *Dispatcher) handleFailure(ctx context.Context, req *WebhookRequest, props *WebhookRequestProperties, errorType, errorCode string, epoch int64) {
	props.ErrorType = errorType
	props.ErrorCode = errorCode

	if props.Retry == RetryForever {
		_ = d.store.UpdateRequestResult(ctx, req.ID, StatusQueued, ResultFail, errorType, errorCode, epoch, props)
	} else {
		_ = d.store.UpdateRequestResult(ctx, req.ID, StatusFailed, ResultFail, errorType, errorCode, epoch, props)
	}
}

func computeHMACSHA256(message, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func phidGetType(phid string) string {
	parts := strings.SplitN(phid, "-", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

func isTimeoutErr(err error) bool {
	type timeouter interface {
		Timeout() bool
	}
	if te, ok := err.(timeouter); ok {
		return te.Timeout()
	}
	return false
}
