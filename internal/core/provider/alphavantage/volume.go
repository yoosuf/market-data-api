package alphavantage

import (
	"fmt"
	"strconv"
	"strings"
)

// parseVolume reads a share count. The provider sends a plain integer, but
// some series carry a trailing ".0000"; that is accepted, anything else is not.
func parseVolume(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if whole, frac, ok := strings.Cut(s, "."); ok {
		if strings.Trim(frac, "0") != "" {
			return 0, fmt.Errorf("fractional volume %q", s)
		}
		s = whole
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed volume %q", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("negative volume %q", s)
	}
	return v, nil
}
