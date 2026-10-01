package service

import "testing"

func TestRateLimitIsNotFailureBudget(t *testing.T) {
	state, increment, delay, code := RetryPolicy("rate_limit", 100, 30)
	if state != "retry_wait" || increment != 0 || delay != 30 || code != "rate_limit" {
		t.Fatal(state, increment, delay, code)
	}
}
func TestRetriesAreBounded(t *testing.T) {
	state, inc, _, code := RetryPolicy("uncertain", 7, 0)
	if state != "failed" || inc != 1 || code != "exhausted" {
		t.Fatal(state, inc, code)
	}
	state, _, delay, _ := RetryPolicy("transient", 2, 0)
	if state != "retry_wait" || delay < 8 || delay > 10 {
		t.Fatal(state, delay)
	}
}
