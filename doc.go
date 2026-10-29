// Package mailmantasks provides the official Go client for Mailman's hosted
// Tasks API, using only the standard library.
//
// Create a Client with New and an API key from Mailman Settings, API & agents,
// Tasks API. Mailman must be open and connected on the user's computer.
//
// Run creates a task and waits for its result. Concurrent waits share a poller
// per group. Poll is a destructive low-level read: results leave the server
// once returned. Waiter cancellation never aborts a poll already in flight;
// Close cancels all requests and may discard responses in flight.
//
// Task times are seconds. The default task timeout is 300 seconds, with a
// maximum of 3600. Every HTTP request has a 15-second total timeout.
//
// See https://getmailman.app/developers and https://getmailman.app/tasks-api.md.
package mailmantasks
