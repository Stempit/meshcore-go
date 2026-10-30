package meshcore

import "testing"

// TestHandleFrameShortRawDataDoesNotPanic pins the robustness rule: a frame
// handler must survive malformed input, because the stream parser accepts any
// frame length ≥ 1 and one panic kills the whole host process.
func TestHandleFrameShortRawDataDoesNotPanic(t *testing.T) {
	c := NewMeshCoreClient(nil)
	for _, frame := range [][]byte{
		{pushCodeRawData},
		{pushCodeRawData, 0x10},
		{pushCodeRawData, 0x10, 0xd0}, // well-formed length: goes the full path
	} {
		c.handleFrame(frame) // must not panic
	}
}
