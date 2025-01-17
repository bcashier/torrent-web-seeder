package dht

import (
	"sync"
	"time"

	"github.com/anacrolix/log"

	"github.com/anacrolix/dht/v2/krpc"
)

// Transaction keeps track of a message exchange between nodes, such as a
// query message and a response message.
type Transaction struct {
	remoteAddr  Addr
	t           string
	onResponse  func(krpc.Msg)
	onTimeout   func()
	onSendError func(error)
	querySender func(
		attempt int, // 1-based
	) error
	queryResendDelay func() time.Duration
	logger           log.Logger
	q                string

	mu          sync.Mutex
	gotResponse bool
	timer       *time.Timer
	retries     int
	lastSend    time.Time
}

func (t *Transaction) handleResponse(m krpc.Msg) {
	t.mu.Lock()
	t.gotResponse = true
	t.mu.Unlock()
	t.onResponse(m)
}

func (t *Transaction) key() transactionKey {
	return transactionKey{
		t.remoteAddr.String(),
		t.t,
	}
}

func (t *Transaction) startResendTimer() {
	t.timer = time.AfterFunc(0, t.resendCallback)
}

const maxTransactionSends = 3

func (t *Transaction) resendCallback() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gotResponse {
		return
	}
	if t.retries == maxTransactionSends {
		go t.onTimeout()
		return
	}
	t.retries++
	if err := t.sendQuery(); err != nil {
		go t.onSendError(err)
		return
	}
	if t.timer.Reset(t.queryResendDelay()) {
		panic("timer should have fired to get here")
	}
}

func (t *Transaction) sendQuery() error {
	if err := t.querySender(t.retries); err != nil {
		return err
	}
	t.lastSend = time.Now()
	return nil
}
