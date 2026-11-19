package proxy

import (
	"github.com/docker/go-metrics"

	prometheus "github.com/distribution/distribution/v3/metrics"
)

var (
	spoolBytes     = prometheus.ProxyNamespace.NewGauge("spool", "The size of blobs held in the spool directory", metrics.Bytes)
	spoolFallbacks = prometheus.ProxyNamespace.NewLabeledCounter("spool_fallbacks", "The number of blob fetches without a spool, which read the upstream once per request", "reason")
)

func init() {
	spoolFallbacks.WithValues("budget").Inc(0)
	spoolFallbacks.WithValues("error").Inc(0)
}
