package ingester

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-kit/log/level"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"go.uber.org/atomic"

	util_log "github.com/grafana/loki/v3/pkg/util/log"
)

const maxMappedFP = 1 << 20 // About 1M fingerprints reserved for mapping.

var separatorString = string([]byte{model.SeparatorByte})

// FpMapper is used to map fingerprints in order to work around fingerprint
// collisions.
type FpMapper struct {
	// highestMappedFP has to be aligned for atomic operations.
	highestMappedFP atomic.Uint64

	mtx sync.RWMutex // Protects mappings.
	// maps original fingerprints to a map of string representations of
	// metrics to the truly unique fingerprint.
	mappings map[model.Fingerprint]map[string]model.Fingerprint

	// Returns existing labels for given fingerprint, if any.
	// Equality check relies on labels.Labels being sorted.
	fpToLabels func(fingerprint model.Fingerprint) labels.Labels
}

// NewFPMapper returns an fpMapper ready to use.
func NewFPMapper(fpToLabels func(fingerprint model.Fingerprint) labels.Labels) *FpMapper {
	if fpToLabels == nil {
		panic("nil fpToLabels")
	}

	return &FpMapper{
		fpToLabels: fpToLabels,
		mappings:   map[model.Fingerprint]map[string]model.Fingerprint{},
	}
}

// MapFP takes a raw fingerprint (as returned by Metrics.FastFingerprint) and
// returns a truly unique fingerprint. The caller must have locked the raw
// fingerprint.
func (m *FpMapper) MapFP(fp model.Fingerprint, metric labels.Labels) model.Fingerprint {
	// First check if we are in the reserved FP space, in which case this is
	// automatically a collision that has to be mapped.
	if fp <= maxMappedFP {
		return m.maybeAddMapping(fp, metric)
	}

	// Then check the most likely case: This fp belongs to a series that is
	// already in memory.
	s := m.fpToLabels(fp)
	if !s.IsEmpty() {
		// FP exists in memory, but is it for the same metric?
		if labels.Equal(metric, s) {
			// Yupp. We are done.
			return fp
		}
		// Collision detected!
		return m.maybeAddMapping(fp, metric)
	}
	// Metric is not in memory. Before doing the expensive archive lookup,
	// check if we have a mapping for this metric in place already.
	m.mtx.RLock()
	mappedFPs, fpAlreadyMapped := m.mappings[fp]
	m.mtx.RUnlock()
	if fpAlreadyMapped {
		// We indeed have mapped fp historically.
		ms := metricToUniqueString(metric)
		// fp is locked by the caller, so no further locking of
		// 'collisions' required (it is specific to fp).
		mappedFP, ok := mappedFPs[ms]
		if ok {
			// Historical mapping found, return the mapped FP.
			return mappedFP
		}
	}
	return fp
}

// maybeAddMapping is only used internally. It takes a detected collision and
// adds it to the collisions map if not yet there. In any case, it returns the
// truly unique fingerprint for the colliding metric.
func (m *FpMapper) maybeAddMapping(fp model.Fingerprint, collidingMetric labels.Labels) model.Fingerprint {
	ms := metricToUniqueString(collidingMetric)
	m.mtx.RLock()
	mappedFPs, ok := m.mappings[fp]
	m.mtx.RUnlock()
	if ok {
		// fp is locked by the caller, so no further locking required.
		mappedFP, ok := mappedFPs[ms]
		if ok {
			return mappedFP // Existing mapping.
		}
		// A new mapping has to be created.
		mappedFP = m.nextMappedFP()
		mappedFPs[ms] = mappedFP
		level.Info(util_log.Logger).Log(
			"msg", "fingerprint collision detected, mapping to new fingerprint",
			"old_fp", fp,
			"new_fp", mappedFP,
			"metric", ms,
		)
		return mappedFP
	}
	// This is the first collision for fp.
	mappedFP := m.nextMappedFP()
	mappedFPs = map[string]model.Fingerprint{ms: mappedFP}
	m.mtx.Lock()
	m.mappings[fp] = mappedFPs
	m.mtx.Unlock()
	level.Info(util_log.Logger).Log(
		"msg", "fingerprint collision detected, mapping to new fingerprint",
		"old_fp", fp,
		"new_fp", mappedFP,
		"metric", collidingMetric,
	)
	return mappedFP
}

func (m *FpMapper) nextMappedFP() model.Fingerprint {
	mappedFP := model.Fingerprint(m.highestMappedFP.Inc())
	if mappedFP > maxMappedFP {
		panic(fmt.Errorf("more than %v fingerprints mapped in collision detection", maxMappedFP))
	}
	return mappedFP
}

// metricToUniqueString turns a metric into a string in a reproducible and
// unique way, i.e. the same metric will always create the same string, and
// different metrics will always create different strings. In a way, it is the
// "ideal" fingerprint function, only that it is more expensive than the
// FastFingerprint function, and its result is not suitable as a key for maps
// and indexes as it might become really large, causing a lot of hashing effort
// in maps and a lot of storage overhead in indexes.
func metricToUniqueString(m labels.Labels) string {
	parts := make([]string, 0, m.Len())
	m.Range(func(l labels.Label) {
		parts = append(parts, l.Name+separatorString+l.Value)
	})
	sort.Strings(parts)
	return strings.Join(parts, separatorString)
}
