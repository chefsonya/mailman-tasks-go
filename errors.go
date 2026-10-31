package mailmantasks

import (
	"fmt"
	"time"
)

// MailmanError describes an HTTP error. RetryAfter is zero when absent.
type MailmanError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *MailmanError) Error() string { return fmt.Sprintf("Tasks API %d: %s", e.Status, e.Message) }

type AuthError struct{ *MailmanError }
type InvalidRequestError struct{ *MailmanError }
type PayloadTooLargeError struct{ *MailmanError }
type RateLimitError struct{ *MailmanError }
type PendingLimitError struct{ *MailmanError }
type ServerBusyError struct{ *MailmanError }
type RequestTimeoutError struct{ *MailmanError }

func (e *AuthError) Unwrap() error            { return e.MailmanError }
func (e *InvalidRequestError) Unwrap() error  { return e.MailmanError }
func (e *PayloadTooLargeError) Unwrap() error { return e.MailmanError }
func (e *RateLimitError) Unwrap() error       { return e.MailmanError }
func (e *PendingLimitError) Unwrap() error    { return e.MailmanError }
func (e *ServerBusyError) Unwrap() error      { return e.MailmanError }
func (e *RequestTimeoutError) Unwrap() error  { return e.MailmanError }

// NetworkError is a transport or response-read failure. Error omits the cause
// text to keep credentials out of messages; Unwrap preserves cause inspection.
// MayHaveBeenAccepted is conservatively true for every failed create request:
// do not automatically retry it, as the task may already exist.
type NetworkError struct {
	Cause               error
	MayHaveBeenAccepted bool
}

func (e *NetworkError) Error() string {
	if e.MayHaveBeenAccepted {
		return "Tasks API transport failed; create may have been accepted"
	}
	return "Tasks API transport failed"
}
func (e *NetworkError) Unwrap() error { return e.Cause }

// TooManyTasksError means client tracking capacity was reached. No create
// request was sent. Slots become available when tracked tasks safely expire.
type TooManyTasksError struct{ Limit int }

func (e *TooManyTasksError) Error() string {
	return fmt.Sprintf("mailman tasks client tracking limit reached (%d tasks); nothing was sent", e.Limit)
}

type TaskFailedError struct{ ID, ErrorStatus, ErrorMessage string }

func (e *TaskFailedError) Error() string {
	return "task " + e.ID + " failed: " + e.ErrorStatus + ": " + e.ErrorMessage
}

// TaskTimeoutError distinguishes a completed timeout from a lost result deadline.
// Reason is "timeout" when no matching email arrived, or "deadline" when no result arrived.
type TaskTimeoutError struct{ ID, Reason string }

func (e *TaskTimeoutError) Error() string {
	if e.Reason == "timeout" {
		return "task " + e.ID + " timed out: no matching email arrived in time."
	}
	return "task " + e.ID + " is lost: no result by the deadline. Create it again."
}
