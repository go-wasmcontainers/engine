package runtime

import "testing"

func TestGuestExitBounded(t *testing.T) {
	for _, tc := range []struct {
		in   uint32
		want int
	}{
		{0, 0}, {1, 1}, {42, 42}, {125, 125},
		{126, 126}, {127, 127}, {130, 130}, {255, 255},
		{256, 1}, {300, 1},
		{0xFFFFFFFF, 1}, // os.Exit(-1), as the u32 of proc_exit
		{0x80000000, 1},
	} {
		r := GuestExit(tc.in)
		if r.ExitCode != tc.want {
			t.Errorf("GuestExit(%d).ExitCode = %d, want %d", tc.in, r.ExitCode, tc.want)
		}
		if r.ExitCode < 0 || r.ExitCode > 255 {
			t.Errorf("GuestExit(%d) = %d is not a Docker status", tc.in, r.ExitCode)
		}
		// A WASIX parent still sees the raw code.
		if st := r.ExitStatus(); st.Code != tc.in || st.Signal != 0 {
			t.Errorf("GuestExit(%d).ExitStatus() = %+v", tc.in, st)
		}
	}
}
