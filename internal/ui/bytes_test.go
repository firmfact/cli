package ui

import "testing"

// Sizes read as the service writes its limits: binary units, one decimal
// at most, none when it is nought.
func TestBytes(t *testing.T) {
	cases := map[int64]string{
		0:             "0 bytes",
		1:             "1 byte",
		512:           "512 bytes",
		1023:          "1023 bytes",
		1024:          "1 KB",
		2535:          "2.5 KB",
		1024*1024 - 1: "1 MB",
		52428799:      "50 MB",
		100 << 20:     "100 MB",
		105906176:     "101 MB",
		126353408:     "120.5 MB",
		3 << 30:       "3 GB",
		5 << 40:       "5 TB",
		9000 << 40:    "9000 TB",
	}
	for n, want := range cases {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}
