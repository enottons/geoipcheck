package app

import (
	"runtime"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
)

const unknownBuild = "unknown"

type buildInfo struct {
	version   string
	revision  string
	goVersion string
}

// readBuildInfo reads what the Go toolchain stamped into the binary. A
// `go build` in a git checkout records the commit, and the version reads
// "(devel)" unless built from a module tag. `go run` and tests record no
// commit, so it reads "unknown". vcs.modified is not exported: CI builds
// from clean checkouts.
func readBuildInfo() buildInfo {
	b := buildInfo{version: unknownBuild, revision: unknownBuild, goVersion: runtime.Version()}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	if v := info.Main.Version; v != "" {
		b.version = v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			b.revision = s.Value
		}
	}
	return b
}

// collector exposes geoipcheck_build_info, a constant 1 whose labels say
// which build is running, so dashboards can tell a rollout apart.
func (b buildInfo) collector() prometheus.Collector {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "geoipcheck_build_info",
		Help: "Always 1. Labels identify the running build.",
		ConstLabels: prometheus.Labels{
			"version":   b.version,
			"revision":  b.revision,
			"goversion": b.goVersion,
		},
	})
	g.Set(1)
	return g
}
