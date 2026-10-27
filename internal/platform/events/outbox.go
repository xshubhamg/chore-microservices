package events

// Outbox is an envelope plus the W3C trace context of the request that produced
// it. The repository stores both in the same transaction as the domain change,
// and the relay uses TraceParent to link the asynchronous publish to the
// originating trace.
type Outbox struct {
	Envelope    Envelope
	TraceParent string
}
