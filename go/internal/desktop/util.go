package desktop

import (
	"net/url"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

func urlEscape(s string) string { return url.PathEscape(s) }
