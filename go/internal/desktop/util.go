package desktop

import "net/url"

func urlEscape(s string) string { return url.PathEscape(s) }
