package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"iot-server/internal/store"
)

const (
	laneCap     = 256
	poisonAfter = 8
)

// delivery 是一条已收到、尚未向 Broker 确认的 MQTT 消息。
type delivery struct {
	topic string
	body  []byte
	hash  string
	ack   func()
	fails int
	noted bool
}

// lane 按收到顺序重试。数据库恢复前不确认。
// 队列有上限。持续失败的消息移出热路径，避免堵住后面的报文，也不再无限占内存。
type lane struct {
	mu       sync.Mutex
	items    []delivery
	poison   []delivery
	wake     chan struct{}
	isolate  func(delivery) bool
	poisonAt time.Time
}

func newLane() *lane {
	return &lane{wake: make(chan struct{}, 1)}
}

func messageHash(topic string, body []byte) string {
	sum := sha256.New()
	_, _ = sum.Write([]byte(topic))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write(body)
	return hex.EncodeToString(sum.Sum(nil))
}

// add 收下一条消息。队列已满时返回 false，调用方不能确认，让 Broker 以后重投。
// 同一条报文已经在队列里时只补上确认函数，不占第二个位置。
func (l *lane) add(item delivery) bool {
	if item.hash == "" {
		item.hash = messageHash(item.topic, item.body)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.absorb(item) {
		return true
	}
	if len(l.items)+len(l.poison) >= laneCap {
		return false
	}
	l.items = append(l.items, item)
	l.notify()
	return true
}

func (l *lane) absorb(item delivery) bool {
	for i := range l.items {
		if l.items[i].hash != item.hash {
			continue
		}
		if l.items[i].ack == nil && item.ack != nil {
			l.items[i].ack = item.ack
		}
		return true
	}
	for i := range l.poison {
		if l.poison[i].hash != item.hash {
			continue
		}
		if l.poison[i].ack == nil && item.ack != nil {
			l.poison[i].ack = item.ack
		}
		return true
	}
	return false
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

func (l *lane) park(item delivery) {
	l.mu.Lock()
	l.poison = append(l.poison, item)
	l.mu.Unlock()
}

func (l *lane) takePoison() (delivery, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.poison) == 0 || time.Now().Before(l.poisonAt) {
		return delivery{}, false
	}
	item := l.poison[0]
	l.poison = l.poison[1:]
	l.poisonAt = time.Now().Add(2 * time.Second)
	return item, true
}

func (l *lane) run(ctx context.Context, handle func(string, []byte) error, retryWait time.Duration, onErr func(error, bool)) {
	if retryWait <= 0 {
		retryWait = 200 * time.Millisecond
	}
	for {
		item, ok := l.pop()
		if !ok {
			item, ok = l.takePoison()
		}
		if !ok {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-l.wake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		err := safeHandle(handle, item.topic, item.body)
		if err == nil {
			l.ack(item)
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
		item.fails++
		if item.fails >= poisonAfter {
			if l.isolate != nil && l.isolate(item) {
				l.ack(item)
				continue
			}
			l.park(item)
			continue
		}
		l.pushBack(item)
		if onErr != nil && first {
			onErr(err, true)
		}
		if !l.wait(ctx, retryWait/4) {
			return
		}
	}
}

func (l *lane) ack(item delivery) {
	if item.ack != nil {
		item.ack()
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
