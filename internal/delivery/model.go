package delivery

const (
	RetryNever   = "never"
	RetryForever = "forever"

	StatusQueued = "queued"
	StatusFailed = "failed"
	StatusSent   = "sent"

	ResultNone = "none"
	ResultOkay = "okay"
	ResultFail = "fail"

	ErrorTypeHook    = "hook"
	ErrorTypeHTTP    = "http"
	ErrorTypeTimeout = "timeout"
)

type WebhookRequest struct {
	ID                int64  `json:"id"`
	PHID              string `json:"phid"`
	WebhookPHID       string `json:"webhookPHID"`
	ObjectPHID        string `json:"objectPHID"`
	Status            string `json:"status"`
	Properties        string `json:"properties"`
	LastRequestResult string `json:"lastRequestResult"`
	LastRequestEpoch  int64  `json:"lastRequestEpoch"`
	DateCreated       int64  `json:"dateCreated"`
	DateModified      int64  `json:"dateModified"`
}

type WebhookRequestProperties struct {
	Retry            string   `json:"retry,omitempty"`
	ErrorType        string   `json:"errorType,omitempty"`
	ErrorCode        string   `json:"errorCode,omitempty"`
	TransactionPHIDs []string `json:"transactionPHIDs,omitempty"`
	TriggerPHIDs     []string `json:"triggerPHIDs,omitempty"`
	Silent           bool     `json:"silent,omitempty"`
	Test             bool     `json:"test,omitempty"`
	Secure           bool     `json:"secure,omitempty"`
}

type Webhook struct {
	ID         int64  `json:"id"`
	PHID       string `json:"phid"`
	Name       string `json:"name"`
	WebhookURI string `json:"webhookURI"`
	Status     string `json:"status"`
	HmacKey    string `json:"hmacKey"`
}

type WebhookPayload struct {
	Object       PayloadObject        `json:"object"`
	Triggers     []PayloadTrigger     `json:"triggers"`
	Action       PayloadAction        `json:"action"`
	Transactions []PayloadTransaction `json:"transactions"`
}

type PayloadObject struct {
	Type string `json:"type"`
	PHID string `json:"phid"`
}

type PayloadTrigger struct {
	PHID string `json:"phid"`
}

type PayloadAction struct {
	Test   bool  `json:"test"`
	Silent bool  `json:"silent"`
	Secure bool  `json:"secure"`
	Epoch  int64 `json:"epoch"`
}

type PayloadTransaction struct {
	PHID string `json:"phid"`
}

type DeliveryStats struct {
	QueuedCount    int64 `json:"queuedCount"`
	SentCount      int64 `json:"sentCount"`
	FailedCount    int64 `json:"failedCount"`
	ActiveWebhooks int64 `json:"activeWebhooks"`
}
