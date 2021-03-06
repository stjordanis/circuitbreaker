package circuit

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/facebookgo/clock"
)

func init() {
	defaultInitialBackOffInterval = time.Millisecond
}

func TestBreakerTripping(t *testing.T) {
	cb := NewBreaker()

	if cb.Tripped() {
		t.Fatal("expected breaker to not be tripped")
	}

	cb.Trip()
	if !cb.Tripped() {
		t.Fatal("expected breaker to be tripped")
	}

	cb.Reset()
	if cb.Tripped() {
		t.Fatal("expected breaker to have been reset")
	}
}

func TestBreakerCounts(t *testing.T) {
	cb := NewBreaker()

	cb.Fail(nil)
	if failures := cb.Failures(); failures != 1 {
		t.Fatalf("expected failure count to be 1, got %d", failures)
	}

	cb.Fail(nil)
	if consecFailures := cb.ConsecFailures(); consecFailures != 2 {
		t.Fatalf("expected 2 consecutive failures, got %d", consecFailures)
	}

	cb.Success()
	if successes := cb.Successes(); successes != 1 {
		t.Fatalf("expected success count to be 1, got %d", successes)
	}
	if consecFailures := cb.ConsecFailures(); consecFailures != 0 {
		t.Fatalf("expected 0 consecutive failures, got %d", consecFailures)
	}

	cb.Reset()
	if failures := cb.Failures(); failures != 0 {
		t.Fatalf("expected failure count to be 0, got %d", failures)
	}
	if successes := cb.Successes(); successes != 0 {
		t.Fatalf("expected success count to be 0, got %d", successes)
	}
	if consecFailures := cb.ConsecFailures(); consecFailures != 0 {
		t.Fatalf("expected 0 consecutive failures, got %d", consecFailures)
	}
}

func TestErrorRate(t *testing.T) {
	cb := NewBreaker()
	if er := cb.ErrorRate(); er != 0.0 {
		t.Fatalf("expected breaker with no samples to have 0 error rate, got %f", er)
	}
}

func TestBreakerEvents(t *testing.T) {
	c := clock.NewMock()
	cb := NewBreaker()
	cb.Clock = c
	events := cb.Subscribe()

	cb.Trip()
	if e := <-events; e != BreakerTripped {
		t.Fatalf("expected to receive a trip event, got %d", e)
	}

	c.Add(cb.nextBackOff + 1)
	cb.Ready()
	if e := <-events; e != BreakerReady {
		t.Fatalf("expected to receive a breaker ready event, got %d", e)
	}

	cb.Reset()
	if e := <-events; e != BreakerReset {
		t.Fatalf("expected to receive a reset event, got %d", e)
	}

	cb.Fail(nil)
	if e := <-events; e != BreakerFail {
		t.Fatalf("expected to receive a fail event, got %d", e)
	}
}

func TestAddRemoveListener(t *testing.T) {
	c := clock.NewMock()
	cb := NewBreaker()
	cb.Clock = c
	events := make(chan ListenerEvent, 100)
	cb.AddListener(events)

	cb.Trip()
	if e := <-events; e.Event != BreakerTripped {
		t.Fatalf("expected to receive a trip event, got %v", e)
	}

	c.Add(cb.nextBackOff + 1)
	cb.Ready()
	if e := <-events; e.Event != BreakerReady {
		t.Fatalf("expected to receive a breaker ready event, got %v", e)
	}

	cb.Reset()
	if e := <-events; e.Event != BreakerReset {
		t.Fatalf("expected to receive a reset event, got %v", e)
	}

	cb.Fail(nil)
	if e := <-events; e.Event != BreakerFail {
		t.Fatalf("expected to receive a fail event, got %v", e)
	}

	cb.RemoveListener(events)
	cb.Reset()
	select {
	case e := <-events:
		t.Fatalf("after removing listener, should not receive reset event; got %v", e)
	default:
		// Expected.
	}
}

func TestTrippableBreakerState(t *testing.T) {
	c := clock.NewMock()
	cb := NewBreaker()
	cb.Clock = c

	if !cb.Ready() {
		t.Fatal("expected breaker to be ready")
	}

	cb.Trip()
	if cb.Ready() {
		t.Fatal("expected breaker to not be ready")
	}
	c.Add(cb.nextBackOff + 1)
	if !cb.Ready() {
		t.Fatal("expected breaker to be ready after reset timeout")
	}

	cb.Fail(nil)
	c.Add(cb.nextBackOff + 1)
	if !cb.Ready() {
		t.Fatal("expected breaker to be ready after reset timeout, post failure")
	}
}

func TestTrippableBreakerManualBreak(t *testing.T) {
	c := clock.NewMock()
	cb := NewBreaker()
	cb.Clock = c
	cb.Break()
	c.Add(cb.nextBackOff + 1)

	if cb.Ready() {
		t.Fatal("expected breaker to still be tripped")
	}

	cb.Reset()
	cb.Trip()
	c.Add(cb.nextBackOff + 1)
	if !cb.Ready() {
		t.Fatal("expected breaker to be ready")
	}
}

func TestThresholdBreaker(t *testing.T) {
	cb := NewThresholdBreaker(2)

	if cb.Tripped() {
		t.Fatal("expected threshold breaker to be open")
	}

	cb.Fail(nil)
	if cb.Tripped() {
		t.Fatal("expected threshold breaker to still be open")
	}

	cb.Fail(nil)
	if !cb.Tripped() {
		t.Fatal("expected threshold breaker to be tripped")
	}

	cb.Reset()
	if failures := cb.Failures(); failures != 0 {
		t.Fatalf("expected reset to set failures to 0, got %d", failures)
	}
	if cb.Tripped() {
		t.Fatal("expected threshold breaker to be open")
	}
}

func TestConsecutiveBreaker(t *testing.T) {
	cb := NewConsecutiveBreaker(3)

	if cb.Tripped() {
		t.Fatal("expected consecutive breaker to be open")
	}

	cb.Fail(nil)
	cb.Success()
	cb.Fail(nil)
	cb.Fail(nil)
	if cb.Tripped() {
		t.Fatal("expected consecutive breaker to be open")
	}
	cb.Fail(nil)
	if !cb.Tripped() {
		t.Fatal("expected consecutive breaker to be tripped")
	}
}

func TestThresholdBreakerCalling(t *testing.T) {
	circuit := func() error {
		return fmt.Errorf("error")
	}

	cb := NewThresholdBreaker(2)

	err := cb.Call(circuit, 0) // First failure
	if err == nil {
		t.Fatal("expected threshold breaker to error")
	}
	if cb.Tripped() {
		t.Fatal("expected threshold breaker to be open")
	}

	err = cb.Call(circuit, 0) // Second failure trips
	if err == nil {
		t.Fatal("expected threshold breaker to error")
	}
	if !cb.Tripped() {
		t.Fatal("expected threshold breaker to be tripped")
	}
}

func TestThresholdBreakerCallingContext(t *testing.T) {
	circuit := func() error {
		return fmt.Errorf("error")
	}

	cb := NewThresholdBreaker(2)
	ctx, cancel := context.WithCancel(context.Background())

	err := cb.CallContext(ctx, circuit, 0) // First failure
	if err == nil {
		t.Fatal("expected threshold breaker to error")
	}
	if cb.Tripped() {
		t.Fatal("expected threshold breaker to be open")
	}

	// Cancel the next Call.
	cancel()

	err = cb.CallContext(ctx, circuit, 0) // Second failure but it's canceled
	if err == nil {
		t.Fatal("expected threshold breaker to error")
	}
	if cb.Tripped() {
		t.Fatal("expected threshold breaker to be open")
	}

	err = cb.CallContext(context.Background(), circuit, 0) // Thirt failure trips
	if err == nil {
		t.Fatal("expected threshold breaker to error")
	}
	if !cb.Tripped() {
		t.Fatal("expected threshold breaker to be tripped")
	}
}

func TestThresholdBreakerResets(t *testing.T) {
	called := 0
	success := false
	circuit := func() error {
		if called == 0 {
			called++
			return fmt.Errorf("error")
		}
		success = true
		return nil
	}

	c := clock.NewMock()
	cb := NewThresholdBreaker(1)
	cb.Clock = c
	err := cb.Call(circuit, 0)
	if err == nil {
		t.Fatal("Expected cb to return an error")
	}

	c.Add(cb.nextBackOff + 1)
	for i := 0; i < 4; i++ {
		err = cb.Call(circuit, 0)
		if err != nil {
			t.Fatal("Expected cb to be successful")
		}

		if !success {
			t.Fatal("Expected cb to have been reset")
		}
	}
}

func TestTimeoutBreaker(t *testing.T) {
	wait := make(chan struct{})

	c := clock.NewMock()
	called := int32(0)

	circuit := func() error {
		wait <- struct{}{}
		atomic.AddInt32(&called, 1)
		<-wait
		return nil
	}

	cb := NewThresholdBreaker(1)
	cb.Clock = c

	errc := make(chan error)
	go func() { errc <- cb.Call(circuit, time.Millisecond) }()

	<-wait
	c.Add(time.Millisecond * 3)
	wait <- struct{}{}

	err := <-errc
	if err == nil {
		t.Fatal("expected timeout breaker to return an error")
	}

	go cb.Call(circuit, time.Millisecond)
	<-wait
	c.Add(time.Millisecond * 3)
	wait <- struct{}{}

	if !cb.Tripped() {
		t.Fatal("expected timeout breaker to be open")
	}
}

func TestRateBreakerTripping(t *testing.T) {
	cb := NewRateBreaker(0.5, 4)
	cb.Success()
	cb.Success()
	cb.Fail(nil)
	cb.Fail(nil)

	if !cb.Tripped() {
		t.Fatal("expected rate breaker to be tripped")
	}

	if er := cb.ErrorRate(); er != 0.5 {
		t.Fatalf("expected error rate to be 0.5, got %f", er)
	}
}

func TestRateBreakerSampleSize(t *testing.T) {
	cb := NewRateBreaker(0.5, 100)
	cb.Fail(nil)

	if cb.Tripped() {
		t.Fatal("expected rate breaker to not be tripped yet")
	}
}

func TestRateBreakerResets(t *testing.T) {
	serviceError := fmt.Errorf("service error")

	called := 0
	success := false
	circuit := func() error {
		if called < 4 {
			called++
			return serviceError
		}
		success = true
		return nil
	}

	c := clock.NewMock()
	cb := NewRateBreaker(0.5, 4)
	cb.Clock = c
	var err error
	for i := 0; i < 4; i++ {
		err = cb.Call(circuit, 0)
		if err == nil {
			t.Fatal("Expected cb to return an error (closed breaker, service failure)")
		} else if err != serviceError {
			t.Fatal("Expected cb to return error from service (closed breaker, service failure)")
		}
	}

	err = cb.Call(circuit, 0)
	if err == nil {
		t.Fatal("Expected cb to return an error (open breaker)")
	} else if err != ErrBreakerOpen {
		t.Fatal("Expected cb to return open open breaker error (open breaker)")
	}

	c.Add(cb.nextBackOff + 1)
	err = cb.Call(circuit, 0)
	if err != nil {
		t.Fatal("Expected cb to be successful")
	}

	if !success {
		t.Fatal("Expected cb to have been reset")
	}
}

func TestRateBreakerResetsOnSuccess(t *testing.T) {
	serviceError := fmt.Errorf("service error")

	called := 0
	success := false
	circuit := func() error {
		if called < 4 {
			called++
			return serviceError
		}
		success = true
		return nil
	}

	c := clock.NewMock()
	cb := NewRateBreaker(0.5, 4)
	cb.Clock = c
	var err error
	for i := 0; i < 4; i++ {
		err = cb.Call(circuit, 0)
		if err == nil {
			t.Fatal("Expected cb to return an error (closed breaker, service failure)")
		} else if err != serviceError {
			t.Fatalf("Expected cb to return error from service; got %v", err)
		}
	}

	err = cb.Call(circuit, 0)
	if err == nil {
		t.Fatal("Expected cb to return an error (open breaker)")
	} else if err != ErrBreakerOpen {
		t.Fatalf("Expected cb to return open open breaker error; got %v", err)
	}

	cb.Success()
	err = cb.Call(circuit, 0)
	if err != nil {
		t.Fatalf("Expected cb to be successful after Success() call; got %v", err)
	}
	if !success {
		t.Fatal("Expected cb to have been reset after Success() call")
	}
}

func TestNeverRetryAfterBackoffStops(t *testing.T) {
	cb := NewBreakerWithOptions(&Options{
		BackOff: &backoff.StopBackOff{},
	})

	cb.Trip()

	// circuit should be open and never retry again
	// when nextBackoff is backoff.Stop
	called := 0
	cb.Call(func() error {
		called = 1
		return nil
	}, 0)

	if called == 1 {
		t.Fatal("Expected cb to never retry")
	}
}

// TestPartialSecondBackoff ensures that the breaker event less than nextBackoff value
// time after tripping the breaker isn't allowed.
func TestPartialSecondBackoff(t *testing.T) {
	c := clock.NewMock()
	cb := NewBreaker()
	cb.Clock = c

	// Set the time to 0.5 seconds after the epoch, then trip the breaker.
	c.Add(500 * time.Millisecond)
	cb.Trip()

	// Move forward 100 milliseconds in time and ensure that the backoff time
	// is set to a larger number than the clock advanced.
	c.Add(100 * time.Millisecond)
	cb.nextBackOff = 500 * time.Millisecond
	if cb.Ready() {
		t.Fatalf("expected breaker not to be ready after less time than nextBackoff had passed")
	}

	c.Add(401 * time.Millisecond)
	if !cb.Ready() {
		t.Fatalf("expected breaker to be ready after more than nextBackoff time had passed")
	}
}

// TestNoDeadlockOnChannelSends ensures that the behavior of channel sends in
// the face of concurrent events and consumers does not lead to deadlock.
func TestNoDeadlockOnChannelSends(t *testing.T) {
	const listeners = 1000
	const subscribers = 1000
	const resetters = 3
	b := NewBreakerWithOptions(nil)
	var lcs []chan ListenerEvent
	for i := 0; i < listeners; i++ {
		lcs = append(lcs, make(chan ListenerEvent, 1))
		b.AddListener(lcs[i])
	}
	var scs []<-chan BreakerEvent
	for i := 0; i < subscribers; i++ {
		scs = append(scs, b.Subscribe())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	readFromSubscribeChan := func(sc <-chan BreakerEvent) {
		defer wg.Done()
		for {
			select {
			case <-sc:
			case <-ctx.Done():
				return
			}
		}
	}
	readFromListenerChan := func(lc chan ListenerEvent) {
		defer wg.Done()
		for {
			select {
			case <-lc:
			case <-ctx.Done():
				return
			}
		}
	}
	tripAndReset := func() {
		defer wg.Done()
		for i := 0; true; i++ {
			// Keep sending a bit after the other goroutine exits.
			if i%1000 == 0 && ctx.Err() != nil {
				return
			}
			b.Reset()
			b.Trip()
		}
	}
	for _, lc := range lcs {
		wg.Add(1)
		go readFromListenerChan(lc)
	}
	for _, sc := range scs {
		wg.Add(1)
		go readFromSubscribeChan(sc)
	}
	for i := 0; i < resetters; i++ {
		wg.Add(1)
		go tripAndReset()
	}
	wg.Wait()
}

// TestLoggerEvents ensures that the name and events get logged to the logger at
// the expected level.
func TestLoggerEvents(t *testing.T) {
	l := &testLogger{}
	name := "foo"
	b := NewBreakerWithOptions(&Options{
		Name:   name,
		Logger: l,
	})
	b.Reset()
	b.Fail(nil)
	verifyLogCall(t, l.infoCalls, 0, name, BreakerReset)
	verifyLogCall(t, l.debugCalls, 0, name, BreakerFail)
}

func verifyLogCall(t *testing.T, calls []logCall, idx int, expectedArgs ...interface{}) {
	if len(calls) < idx+1 {
		t.Errorf("expected at least %d log calls but only have %d", idx+1, len(calls))
	} else if !reflect.DeepEqual(calls[idx].args, expectedArgs) {
		t.Errorf("expected logging to have been called with %v, got %v", expectedArgs, calls[idx].args)
	}
}

// TestLoggerCallErrors ensures that the name and events get logged to the
// logger at the expected level.
func TestLoggerCallErrors(t *testing.T) {
	l := &testLogger{}
	name := "foo"
	tripNext := false
	b := NewBreakerWithOptions(&Options{
		Name:   name,
		Logger: l,
		ShouldTrip: func(_ *Breaker) bool {
			if tripNext {
				return true
			}
			tripNext = true
			return false
		},
	})
	failErr := fmt.Errorf("boom")
	tripErr := fmt.Errorf("yowza")
	b.Call(func() error { return failErr }, time.Minute)
	b.Call(func() error { return tripErr }, time.Minute)
	verifyLogCall(t, l.debugCalls, 0, name, BreakerFail)
	verifyLogCall(t, l.debugCalls, 1, name, failErr)
	verifyLogCall(t, l.debugCalls, 2, name, BreakerFail)
	verifyLogCall(t, l.infoCalls, 0, name, tripErr)
	verifyLogCall(t, l.infoCalls, 1, name, BreakerTripped)
}

type logCall struct {
	format string
	args   []interface{}
}

type testLogger struct {
	infoCalls  []logCall
	debugCalls []logCall
}

func (l *testLogger) Infof(format string, args ...interface{}) {
	l.infoCalls = append(l.infoCalls, logCall{format, args})
}

func (l *testLogger) Debugf(format string, args ...interface{}) {
	l.debugCalls = append(l.debugCalls, logCall{format, args})
}
