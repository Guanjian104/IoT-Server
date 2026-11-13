package bridge

import (
	"context"
	"errors"
	"sync"
	"time"

	"iot-server/internal/store"
)

// delivery 是一条已收到、尚未向 Broker 确认的 MQTT 消息。
type delivery struct {
	topic string
	body  []byte
	ack   func()
	noted bool
}

// lane 按收到顺序重试。数据库恢复前不确认，避免消息只留在当前会话的在途窗口里。
type lane struct {
	mu    sync.Mutex
	items []delivery
	wake  chan struct{}
}

func newLane() *lane {
	return &lane{wake: make(chan struct{}, 1)}
}

func (l *lane) add(item delivery) {
	l.mu.Lock()
	l.items = append(l.items, item)
	l.mu.Unlock()
	l.notify()
}

func (l *lane) notify() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *lane) pop() (delivery, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.items) == 0 {
		return delivery{}, false
	}
	item := l.items[0]
	l.items = l.items[1:]
	return item, true
}

func (l *lane) pushFront(item delivery) {
	l.mu.Lock()
	l.items = append([]delivery{item}, l.items...)
	l.mu.Unlock()
}

func (l *lane) pushBack(item delivery) {
	l.mu.Lock()
	l.items = append(l.items, item)
	l.mu.Unlock()
}

func (l *lane) run(ctx context.Context, handle func(string, []byte) error, retryWait time.Duration, onErr func(error, bool)) {
	if retryWait <= 0 {
		retryWait = 200 * time.Millisecond
	}
	for {
		item, ok := l.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-l.wake:
			}
			continue
		}
		err := safeHandle(handle, item.topic, item.body)
		if err == nil {
			if item.ack != nil {
				item.ack()
			}
			continue
		}
		first := !item.noted
		item.noted = true
		if errors.Is(err, store.ErrUnavailable) {
			l.pushFront(item)
			if onErr != nil {
				onErr(err, first)
			}
			if !l.wait(ctx, retryWait) {
				return
			}
			continue
		}
		l.pushBack(item)
		if !l.wait(ctx, retryWait/4) {
			return
		}
	}
}

func (l *lane) wait(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		delay = time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func safeHandle(handle func(string, []byte) error, topic string, body []byte) (err error) {
	defer func() {
		if recover() != nil {
			err = store.ErrUnavailable
		}
	}()
	return handle(topic, body)
}
