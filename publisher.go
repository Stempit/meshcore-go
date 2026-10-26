package meshcore

// PacketPublisher is an optional sink for mesh RF packets. Attach one with
// SetPacketPublisher to mirror received and transmitted packets to an external
// system (for example the MQTT bridge). The driver works fine with no publisher
// attached — every publish call is guarded by a nil check.
//
// Keeping this an interface (rather than importing a concrete bridge) means the
// MeshCore driver has no dependency on the MQTT package: the composition root
// (cmd/bot) constructs the bridge and wires it in, and the driver can be built
// and used without MQTT at all.
type PacketPublisher interface {
	// QueuePacket enqueues a raw RF packet for publishing.
	QueuePacket(rawData []byte, isTX bool, snr, rssi float64)
	// QueueChannelPacket enqueues a raw RF channel packet together with its
	// decoded fields for publishing.
	QueueChannelPacket(rawData []byte, isTX bool, snr, rssi float64, channelIndex int, channelName, senderName, text, channelKeyHex string)
	// DevicePrivKey returns the device private key (hex) used to build synthetic
	// outgoing-DM packets for publishing, or "" if unavailable.
	DevicePrivKey() string
	// TxEnabled reports whether the operator's own outgoing messages should be
	// mirrored to MQTT. When false, the synthetic outgoing-packet paths skip
	// publishing entirely.
	TxEnabled() bool
	// Stop shuts the publisher down and releases its resources.
	Stop()
}

// SetPacketPublisher attaches a packet publisher. Passing nil detaches the
// current one (after stopping it).
func (c *MeshCoreClient) SetPacketPublisher(p PacketPublisher) {
	c.mu.Lock()
	old := c.publisher
	c.publisher = p
	c.mu.Unlock()
	if old != nil {
		old.Stop()
	}
}

// StopPublisher stops and detaches the attached packet publisher, if any.
func (c *MeshCoreClient) StopPublisher() {
	c.SetPacketPublisher(nil)
}
