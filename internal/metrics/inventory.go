package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"flash-sale/internal/flashsale"
)

const collectTimeout = 2 * time.Second

// InventoryCollector reads the per-SKU counters at scrape time instead of
// mirroring them into gauges. A SKU that leaves the registry simply stops being
// emitted, so no series can go stale and nothing has to be deleted by hand.
type InventoryCollector struct {
	rdb     redis.Cmdable
	logger  *slog.Logger
	stock   *prometheus.Desc
	pending *prometheus.Desc
}

func NewInventoryCollector(rdb redis.Cmdable, logger *slog.Logger) *InventoryCollector {
	return &InventoryCollector{
		rdb:    rdb,
		logger: logger,
		stock: prometheus.NewDesc(namespace+"_stock_remaining",
			"Units still available for a SKU, read from Redis at scrape time.",
			[]string{"sku"}, nil),
		pending: prometheus.NewDesc(namespace+"_stream_pending",
			"Units reserved but not yet persisted by a worker.",
			[]string{"sku"}, nil),
	}
}

func (c *InventoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.stock
	ch <- c.pending
}

func (c *InventoryCollector) Collect(ch chan<- prometheus.Metric) {
	// Collect carries no context of its own, and a stalled Redis should cost one
	// scrape rather than hold the handler open until Prometheus gives up.
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	levels, err := flashsale.StockLevels(ctx, c.rdb)
	if err != nil {
		c.logger.Warn("collect stock levels failed", "err", err)
		return
	}
	for _, level := range levels {
		ch <- prometheus.MustNewConstMetric(c.stock, prometheus.GaugeValue, float64(level.Remaining), level.SKU)
		ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(level.Queued), level.SKU)
	}
}
