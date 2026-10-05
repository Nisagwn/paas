package web

import (
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 20: the Analitik tab. It renders the Faz 19 analytics (api.Analytics,
// api.Health, api.Usage) on the server: totals, an inline SVG chart of
// requests and 5xx responses per step (no JavaScript), deployment health
// and live CPU / memory.

type rangeOption struct {
	ID, Label string
	Active    bool
}

var rangeLabels = map[string]string{"1h": "1 sa", "24h": "24 sa", "7d": "7 gün"}

type usageRow struct {
	api.DeploymentUsageView
	CPULimit    float64
	MemoryLimit int64
}

type analyticsData struct {
	Range  string
	Ranges []rangeOption
	View   api.AnalyticsView
	Chart  template.HTML
	Health api.HealthView
	// Usage is nil when there is no usage data; UsageNote says why.
	Usage     []usageRow
	UsageNote string
}

func (s *Server) analyticsPage(w http.ResponseWriter, r *http.Request) {
	s.renderTab(w, r, http.StatusOK, tabAnalytics, "", nil)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) analytics(r *http.Request, app store.App) (*analyticsData, error) {
	rng := r.URL.Query().Get("range")
	if _, ok := rangeLabels[rng]; !ok {
		rng = "24h"
	}
	a := &analyticsData{Range: rng}
	for _, id := range api.AnalyticsRanges {
		a.Ranges = append(a.Ranges, rangeOption{ID: id, Label: rangeLabels[id], Active: id == rng})
	}
	now := s.now()
	var err error
	if a.View, err = api.Analytics(r.Context(), s.Store, app.ID, rng, 0, now); err != nil {
		return nil, err
	}
	a.Chart = chartSVG(a.View)
	if a.Health, err = api.Health(r.Context(), s.Store, app.ID, now); err != nil {
		return nil, err
	}
	usage, err := api.Usage(r.Context(), s.Store, s.Usage, app)
	switch {
	case errors.Is(err, api.ErrNoUsage):
		a.UsageNote = "Kullanım verisi yok: canlı CPU ve bellek yalnızca Kubernetes ile çalışırken okunur."
	case errors.Is(err, deploy.ErrNoMetricsAPI):
		a.UsageNote = "Kullanım verisi yok: kümede metrics-server çalışmıyor."
	case err != nil:
		s.Log.Warn("web: usage", "app", app.Name, "err", err)
		a.UsageNote = "Kullanım verisi yok: metrikler şu an okunamadı."
	default:
		a.Usage = []usageRow{}
		for _, d := range usage.Deployments {
			row := usageRow{DeploymentUsageView: d}
			for _, p := range d.Pods {
				row.CPULimit += p.CPULimitMillicores
				row.MemoryLimit += p.MemoryLimitBytes
			}
			a.Usage = append(a.Usage, row)
		}
	}
	return a, nil
}

// Chart geometry (SVG user units; the SVG scales to the card's width).
const (
	chartW      = 720
	chartH      = 200
	chartLeft   = 48
	chartRight  = 8
	chartTop    = 10
	chartBottom = 24
)

// chartSVG draws requests (bars) and 5xx responses (bars in the error
// color, from the same baseline) of every step of v on one count axis.
// Every step has a <title>, the browser's tooltip.
func chartSVG(v api.AnalyticsView) template.HTML {
	n := len(v.Series)
	var max int64
	for _, p := range v.Series {
		if p.Requests > max {
			max = p.Requests
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s">`,
		chartW, chartH, template.HTMLEscapeString(fmt.Sprintf("İstekler ve 5xx hataları, %s", rangeLabels[v.Range])))
	plotW := float64(chartW - chartLeft - chartRight)
	plotH := float64(chartH - chartTop - chartBottom)
	base := float64(chartH - chartBottom)
	if n == 0 || max == 0 {
		fmt.Fprintf(&b, `<line class="axis" x1="%d" y1="%.1f" x2="%d" y2="%.1f"/>`, chartLeft, base, chartW-chartRight, base)
		fmt.Fprintf(&b, `<text class="empty-label" x="%.1f" y="%.1f" text-anchor="middle">Bu aralıkta istek yok</text>`,
			float64(chartLeft)+plotW/2, float64(chartTop)+plotH/2)
		b.WriteString(`</svg>`)
		return template.HTML(b.String())
	}
	top := niceCeil(max)
	// Grid: 0, half, top.
	for _, f := range []float64{0, 0.5, 1} {
		if f == 0.5 && top%2 != 0 {
			continue // no fractional labels
		}
		y := base - f*plotH
		cls := "grid"
		if f == 0 {
			cls = "axis"
		}
		fmt.Fprintf(&b, `<line class="%s" x1="%d" y1="%.1f" x2="%d" y2="%.1f"/>`, cls, chartLeft, y, chartW-chartRight, y)
		fmt.Fprintf(&b, `<text class="tick" x="%d" y="%.1f" text-anchor="end">%s</text>`,
			chartLeft-6, y+4, number(int64(math.Round(f*float64(top)))))
	}
	slot := plotW / float64(n)
	gap := math.Min(2, slot*0.25)
	barW := slot - gap
	layout := "15:04"
	if v.Range == "7d" {
		layout = "02.01 15:04"
	}
	for i, p := range v.Series {
		x := float64(chartLeft) + float64(i)*slot + gap/2
		h := float64(p.Requests) / float64(top) * plotH
		eh := float64(p.Errors) / float64(top) * plotH
		fmt.Fprintf(&b, `<g><title>%s · %s istek · %s hata (5xx)</title>`,
			p.Time.UTC().Format(layout), number(p.Requests), number(p.Errors))
		// A transparent full-height target makes the tooltip easy to hit.
		fmt.Fprintf(&b, `<rect class="hit" x="%.2f" y="%d" width="%.2f" height="%.1f"/>`, x, chartTop, barW, plotH)
		if p.Requests > 0 {
			fmt.Fprintf(&b, `<rect class="bar" x="%.2f" y="%.2f" width="%.2f" height="%.2f"/>`, x, base-h, barW, math.Max(h, 1))
		}
		if p.Errors > 0 {
			fmt.Fprintf(&b, `<rect class="bar-err" x="%.2f" y="%.2f" width="%.2f" height="%.2f"/>`, x, base-eh, barW, math.Max(eh, 1))
		}
		b.WriteString(`</g>`)
	}
	// Time labels: first, middle and last step (UTC).
	for _, i := range []int{0, n / 2, n - 1} {
		x := float64(chartLeft) + (float64(i)+0.5)*slot
		anchor := "middle"
		switch i {
		case 0:
			anchor, x = "start", float64(chartLeft)
		case n - 1:
			anchor, x = "end", float64(chartW-chartRight)
		}
		fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%d" text-anchor="%s">%s</text>`,
			x, chartH-6, anchor, v.Series[i].Time.UTC().Format(layout))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// niceCeil rounds max up to 1, 2 or 5 × 10^k so the axis labels are round.
func niceCeil(max int64) int64 {
	if max <= 1 {
		return 1
	}
	p := int64(1)
	for p*10 < max {
		p *= 10
	}
	for _, m := range []int64{1, 2, 5, 10} {
		if m*p >= max {
			return m * p
		}
	}
	return 10 * p
}
