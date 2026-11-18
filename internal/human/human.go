// Package human formats sizes for people.
package human

import "fmt"

// Bytes formats n as "512 B", "1.5 KiB", "3.2 GiB"...
func Bytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for m := n / u; m >= u; m /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
