package proxy

import (
	"github.com/docker/go-metrics"

	prometheus "github.com/distribution/distribution/v3/metrics"
)

var (
	spoolBytes     = prometheus.ProxyNamespace.NewGauge("spool", "The size of blobs held in the spool directory", metrics.Bytes)
	spoolFallbacks = prometheus.ProxyNamespace.NewLabeledCounter("spool_fallbacks", "The number of blob fetches without a spool, which read the upstream once per request", "reason")
	// In the default mode every request reads the upstream, so followers count duplicate fetches.
	inflightRequests = prometheus.ProxyNamespace.NewLabeledGauge("blob_inflight_requests", "The number of blob requests served on a cache miss", "", "mode", "role", "source")
)

const (
	modeDefault     = "default"
	modeSharedFetch = "sharedfetch"
	roleLeader      = "leader"
	roleFollower    = "follower"
	sourceUpstream  = "upstream"
	sourceSpool     = "spool"
)

func init() {
	spoolFallbacks.WithValues("budget").Inc(0)
	spoolFallbacks.WithValues("error").Inc(0)
	for _, role := range []string{roleLeader, roleFollower} {
		inflightRequests.WithValues(modeDefault, role, sourceUpstream).Set(0)
		inflightRequests.WithValues(modeSharedFetch, role, sourceUpstream).Set(0)
		inflightRequests.WithValues(modeSharedFetch, role, sourceSpool).Set(0)
	}
}

func trackInflight(mode, role, source string) func() {
	gauge := inflightRequests.WithValues(mode, role, source)
	gauge.Inc()
	return func() { gauge.Dec() }
}
