package enter

import "testing"

func TestRawInputMode(t *testing.T) {
	const preserved = uint32(0x0020) // ENABLE_INSERT_MODE, unrelated to raw conversion
	for _, tc := range []struct {
		name string
		in   uint32
		want uint32
	}{
		{
			name: "existing interactive console flags",
			in:   enableProcessedInput | enableLineInput | enableEchoInput | enableMouseInput | enableWindowInput | enableQuickEdit | preserved,
			want: enableExtendedFlags | enableVTInput | preserved,
		},
		{
			name: "mouse and window only",
			in:   enableMouseInput | enableWindowInput,
			want: enableExtendedFlags | enableVTInput,
		},
		{
			name: "zero",
			in:   0,
			want: enableExtendedFlags | enableVTInput,
		},
		{
			name: "already raw",
			in:   enableExtendedFlags | enableVTInput | preserved,
			want: enableExtendedFlags | enableVTInput | preserved,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawInputMode(tc.in); got != tc.want {
				t.Fatalf("rawInputMode(%#x) = %#x, want %#x", tc.in, got, tc.want)
			}
		})
	}
}
