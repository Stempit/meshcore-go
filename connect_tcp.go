package meshcore

// ConnectTCP connects to a MeshCore companion device over TCP and initialises
// the MeshCore protocol over the socket. addr is "host:port" — stock WiFi and
// Ethernet companion firmware serves the same protocol as BLE/USB serial on
// TCP port 5000 (the device is the server; it accepts a single client, and a
// new connection silently replaces the previous one).
func (c *MeshCoreClient) ConnectTCP(addr string) error {
	return c.startWith(
		func() (Connection, error) {
			stream, err := dialTCP(addr)
			if err != nil {
				return nil, err
			}
			c.logger.Info("TCP connection established", "addr", addr)
			return newStreamTransport(stream, c.frameSink(), c.connectionLost, c.logger), nil
		},
		func() error { return c.ConnectTCP(addr) },
	)
}

// CreateTCPClient connects to the companion device at addr (e.g.
// "192.168.1.50:5000"), performs the MeshCore initialisation handshake, and
// returns a ready-to-use MeshCoreClient. It logs and panics when the transport
// itself cannot be established, mirroring CreateSerialClient.
func CreateTCPClient(addr string) *MeshCoreClient {
	client, selfInfoReceived := newSyncedClient()

	if err := client.ConnectTCP(addr); err != nil {
		log().Error("Failed to connect to TCP device", "addr", addr, "error", err)
		panic(err)
	}

	runStartupSync(client, selfInfoReceived, "TCP")

	return client
}
