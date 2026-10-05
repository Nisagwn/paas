package web

import (
	"fmt"
	"strings"
)

// Number formats of the Analitik tab (Faz 20), Turkish style.

// number formats n with thousands separators: 12.345.
func number(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// decimal formats f with one decimal and a comma: 1,5.
func decimal(f float64) string {
	return strings.Replace(fmt.Sprintf("%.1f", f), ".", ",", 1)
}

// pct formats a ratio as a percentage: %1,5.
func pct(ratio float64) string { return "%" + decimal(ratio*100) }

// millis formats a latency in milliseconds; nil (no histogram) is "—".
func millis(ms *float64) string {
	switch {
	case ms == nil:
		return "—"
	case *ms >= 1000:
		return decimal(*ms/1000) + " sn"
	case *ms < 10:
		return decimal(*ms) + " ms"
	}
	return fmt.Sprintf("%.0f ms", *ms)
}

// byteSize formats memory: 120 MiB, 1,5 GiB.
func byteSize(n int64) string {
	const mi = 1 << 20
	switch {
	case n >= 1<<30:
		return decimal(float64(n)/(1<<30)) + " GiB"
	case n >= mi:
		return fmt.Sprintf("%.0f MiB", float64(n)/mi)
	}
	return fmt.Sprintf("%.0f KiB", float64(n)/1024)
}

// cpuLabel formats millicores: 250m, 1,5 çekirdek.
func cpuLabel(m float64) string {
	if m >= 1000 {
		return decimal(m/1000) + " çekirdek"
	}
	return fmt.Sprintf("%.0fm", m)
}
