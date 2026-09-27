package ui

import (
	"math"
	"strconv"
	"strings"
)

// Bytes writes a size the way the firmfact service writes its upload
// limits: binary units with at most one decimal, and none when it is
// nought (512 bytes, 2.5 KB, 50 MB, 100 MB), so that a file the CLI
// reports as 50 MB is the one the server's "50 MB" limit means.
func Bytes(n int64) string {
	switch {
	case n == 1:
		return "1 byte"
	case n < 1024:
		return strconv.FormatInt(n, 10) + " bytes"
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	i := -1
	// Rounded to one decimal, 1023.96 KB would read 1024 KB: that is 1 MB.
	for i+1 < len(units) && math.Round(v*10)/10 >= 1024 {
		v /= 1024
		i++
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + " " + units[i]
}
