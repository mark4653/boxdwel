package enter

// Console input mode bits shared by the Windows console setup and its tests.
const (
	enableProcessedInput = 0x0001
	enableLineInput      = 0x0002
	enableEchoInput      = 0x0004
	enableWindowInput    = 0x0008
	enableMouseInput     = 0x0010
	enableQuickEdit      = 0x0040
	enableExtendedFlags  = 0x0080
	enableVTInput        = 0x0200
)

// rawInputMode computes the raw interactive mode for a plain serial shell.
// Mouse and window events are disabled because the guest shell never consumes
// them; leaving them on floods stdin with coordinate bytes on mouse movement.
func rawInputMode(mode uint32) uint32 {
	return (mode &^ uint32(enableProcessedInput|enableLineInput|enableEchoInput|enableQuickEdit|
		enableMouseInput|enableWindowInput)) | enableExtendedFlags | enableVTInput
}
