package sqldb

import (
	"fmt"
	"net/url"
	"strings"
)

// mysqlURLToDSN converts a mysql:// URL into the DSN go-sql-driver wants.
//
// The driver does not accept URLs -- it wants user:pass@tcp(host:port)/db --
// but a URL is what people write and what every other provider here takes, so
// drover accepts both and converts.
func mysqlURLToDSN(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse mysql url: %w", err)
	}

	var b strings.Builder
	if u.User != nil {
		b.WriteString(u.User.Username())
		if pw, ok := u.User.Password(); ok {
			b.WriteString(":")
			b.WriteString(pw)
		}
		b.WriteString("@")
	}

	host := u.Host
	if host == "" {
		host = "127.0.0.1:3306"
	} else if !strings.Contains(host, ":") {
		host += ":3306"
	}
	b.WriteString("tcp(")
	b.WriteString(host)
	b.WriteString(")")

	db := strings.TrimPrefix(u.Path, "/")
	b.WriteString("/")
	b.WriteString(db)

	if q := u.RawQuery; q != "" {
		b.WriteString("?")
		b.WriteString(q)
	}
	return b.String(), nil
}
