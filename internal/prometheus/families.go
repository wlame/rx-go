package prometheus

import (
	"regexp"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// inventoryRegisterer is a prometheus.Registerer that writes down the
// name of every family registered through it before handing the
// collector on to the wrapped registerer.
//
// Why it exists: Registry.Gather, and so /metrics, leaves out a labeled
// family (a CounterVec, a HistogramVec) until its first label set is
// used. A family that nothing ever updates is therefore invisible in a
// scrape, which is exactly the defect a test wants to catch. This
// wrapper keeps a complete inventory, independent of what has been
// observed.
//
// Embedding prometheus.Registerer (an interface) gives the struct every
// method of the wrapped value; only the two registering methods are
// overridden. promauto calls MustRegister for every declaration.
type inventoryRegisterer struct {
	prometheus.Registerer
}

// Register records c's family names after the wrapped registerer
// accepted c; a rejected collector is not part of the inventory.
func (r inventoryRegisterer) Register(c prometheus.Collector) error {
	if err := r.Registerer.Register(c); err != nil {
		return err
	}
	recordFamilies(c)
	return nil
}

// MustRegister records the names of cs and registers them, panicking on
// failure as the wrapped registerer does.
func (r inventoryRegisterer) MustRegister(cs ...prometheus.Collector) {
	r.Registerer.MustRegister(cs...)
	for _, c := range cs {
		recordFamilies(c)
	}
}

// fqNamePattern extracts the family name from prometheus.Desc.String,
// which renders as `Desc{fqName: "rx_...", help: ...}`. Desc has no
// accessor for the name, and its String layout is what the client
// library prints in its own registration errors.
var fqNamePattern = regexp.MustCompile(`fqName: "([^"]+)"`)

// inventory holds the recorded family names in declaration order. The
// mutex guards it because Register may be called after package
// initialization, from any goroutine.
var inventory struct {
	mu    sync.Mutex
	names []string
}

// recordFamilies appends the family names c describes.
//
// Collector.Describe sends every descriptor on the channel it is given
// and returns; it does not close the channel. Running it in its own
// goroutine and closing the channel afterwards lets the range loop
// below read the descriptors as they come and end when they stop.
func recordFamilies(c prometheus.Collector) {
	descs := make(chan *prometheus.Desc)
	go func() {
		c.Describe(descs)
		close(descs)
	}()
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	for d := range descs {
		if m := fqNamePattern.FindStringSubmatch(d.String()); m != nil {
			inventory.names = append(inventory.names, m[1])
		}
	}
}

// FamilyNames returns the name of every rx_* family this package
// declares, whether or not it has been updated, in declaration order.
// The Go runtime and process collectors are registered on Registry
// directly and are not part of it.
func FamilyNames() []string {
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	return append([]string(nil), inventory.names...)
}
