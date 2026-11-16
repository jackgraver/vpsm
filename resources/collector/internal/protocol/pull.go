package protocol

// # Pull transport
//
// The collector does not have to run all the time. Agents spool every sample
// to their own disk, and the collector fetches the backlog whenever it does
// run, by executing the agent over SSH:
//
//	ssh <host> mini-bot export [--follow]
//
// Pull rather than push because the collector is the end that comes and
// goes: typically a laptop, behind NAT, which a VPS cannot reach but which
// can always reach the VPS. SSH because it is already there — the operator's
// existing login is the authentication, no port is opened on either end,
// and the agent needs no network code at all.
//
// The payload is unchanged: each frame carries an ordinary Report, which
// goes through exactly the validation and storage path a POST does.
//
// # The stream
//
// The agent writes one JSON PullFrame per line to stdout. The collector
// writes lines of the form
//
//	ack <cursor>
//
// to the agent's stdin once everything up to that cursor is stored, and the
// agent deletes the spooled samples the ack covers. Nothing is deleted on
// read, so a transfer cut off midway is simply sent again next time; the
// (host, timestamp) idempotency rule turns the repeats into duplicates.
//
// After the backlog the agent sends a frame with CaughtUp set. A one-shot
// pull acks and closes stdin there, which ends the session. A follow keeps
// reading frames as the agent samples, acking as it goes, until it closes
// stdin itself.

// PullFrame is one line of `mini-bot export` output.
type PullFrame struct {
	// Report is present on data frames.
	Report *Report `json:"report,omitempty"`

	// Cursor is the agent's spool position for the newest sample in
	// Report: the value to ack once the report is stored. It is a sequence
	// number, not a timestamp, so a clock stepped backwards cannot make an
	// ack cover samples the collector never received.
	Cursor uint64 `json:"cursor,omitempty"`

	// CaughtUp marks the end of the backlog.
	CaughtUp bool `json:"caught_up,omitempty"`
}
