package meshcore

import "time"

// HistoryLogger is an optional sink for conversation history. Attach one with
// SetHistoryLogger to persist every incoming and outgoing message to
// per-conversation files. The driver works fine with no logger attached — every
// call is guarded by a nil check.
//
// Keeping this an interface (rather than importing a concrete logger) means the
// MeshCore driver has no dependency on the historylog package: the composition
// root (cmd/bot) constructs the logger and wires it in, and the driver can be
// built and used without history logging at all.
type HistoryLogger interface {
	// LogDMIncoming records a direct message received from a contact.
	LogDMIncoming(contactKey []byte, senderName, text string, t time.Time) error
	// LogDMOutgoing records a direct message sent to a contact.
	LogDMOutgoing(contactKey []byte, text string, t time.Time) error
	// LogChannelIncoming records a message received in a channel.
	LogChannelIncoming(channelName, senderName, text string, t time.Time) error
	// LogChannelOutgoing records a message sent to a channel.
	LogChannelOutgoing(channelName, text string, t time.Time) error
}

// SetHistoryLogger attaches a history logger that records every incoming and
// outgoing message to per-conversation files. Pass a nil interface (not a nil
// concrete pointer) to leave history logging disabled.
func (c *MeshCoreClient) SetHistoryLogger(h HistoryLogger) {
	c.histLog = h
}
